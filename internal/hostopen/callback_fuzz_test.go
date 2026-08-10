package hostopen

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func FuzzCallbackValidation(f *testing.F) {
	for selector := byte(0); selector < 14; selector++ {
		f.Add(selector, "authorization-code")
	}
	f.Add(byte(0), "\x1b]52;c;dG9rZW4=\a")
	f.Add(byte(5), string([]byte{0xff, 0xfe}))

	f.Fuzz(func(t *testing.T, selector byte, code string) {
		if len(code) > 1024 {
			t.Skip()
		}
		const (
			callbackPath  = "/oauth/callback/token"
			callbackHost  = "127.0.0.1:49152"
			expectedState = "expected-state"
		)
		lease := &CallbackLease{
			state:   expectedState,
			path:    callbackPath,
			host:    callbackHost,
			outcome: make(chan callbackOutcome, 1),
		}
		query := url.Values{"state": {expectedState}, "code": {code}}
		request := &http.Request{
			Method:        http.MethodGet,
			URL:           &url.URL{Path: callbackPath, RawQuery: query.Encode()},
			RequestURI:    callbackPath + "?" + query.Encode(),
			Proto:         "HTTP/1.1",
			Host:          callbackHost,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("")),
			ContentLength: 0,
		}

		wantStatus := http.StatusOK
		caseNumber := selector % 14
		switch caseNumber {
		case 0: // Exact request.
		case 1:
			request.Method = http.MethodPost
			wantStatus = http.StatusMethodNotAllowed
		case 2:
			request.URL.Path += "/other"
			wantStatus = http.StatusNotFound
		case 3:
			request.Host = "localhost:49152"
			wantStatus = http.StatusNotFound
		case 4:
			request.URL.RawQuery = "code=" + url.QueryEscape(code)
			wantStatus = http.StatusForbidden
		case 5:
			request.URL.RawQuery += "&state=" + url.QueryEscape(code)
			wantStatus = http.StatusForbidden
		case 6:
			request.URL.RawQuery = "state=wrong&code=" + url.QueryEscape(code)
			wantStatus = http.StatusForbidden
		case 7:
			request.URL.RawPath = callbackPath
			wantStatus = http.StatusNotFound
		case 8:
			request.URL.RawQuery = strings.Repeat("q", maxCallbackQuery+1)
			wantStatus = http.StatusRequestURITooLong
		case 9:
			request.Body = io.NopCloser(strings.NewReader("x"))
			request.ContentLength = 1
			wantStatus = http.StatusBadRequest
		case 10:
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", maxCallbackBody+1)))
			request.ContentLength = maxCallbackBody + 1
			wantStatus = http.StatusRequestEntityTooLarge
		case 11:
			request.RequestURI = strings.Repeat("/", maxCallbackRequestLine+1)
			wantStatus = http.StatusRequestURITooLong
		case 12:
			request.Header.Set("X-Fuzz", strings.Repeat("x", maxCallbackHeaders+1))
			wantStatus = http.StatusRequestHeaderFieldsTooLarge
		case 13: // A second exact request must lose the one-shot CAS.
			first := httptest.NewRecorder()
			lease.ServeHTTP(first, request.Clone(request.Context()))
			if first.Code != http.StatusOK {
				t.Fatalf("first one-shot callback status = %d, want %d", first.Code, http.StatusOK)
			}
			wantStatus = http.StatusConflict
		}

		recorder := httptest.NewRecorder()
		lease.ServeHTTP(recorder, request)
		if recorder.Code != wantStatus {
			t.Fatalf("callback selector %d status = %d, want %d", selector, recorder.Code, wantStatus)
		}
		accepted := caseNumber == 0 || caseNumber == 13
		if lease.accepted.Load() != accepted {
			t.Fatalf("callback selector %d accepted=%t, want %t", selector, lease.accepted.Load(), accepted)
		}
		if accepted {
			select {
			case outcome := <-lease.outcome:
				if outcome.err != nil || outcome.result.Query.Has("state") || outcome.result.Query.Get("code") != code {
					t.Fatalf("accepted callback outcome = %#v", outcome)
				}
			default:
				t.Fatal("accepted callback did not publish an outcome")
			}
			return
		}
		select {
		case outcome := <-lease.outcome:
			t.Fatalf("rejected callback mutated lease outcome: %#v", outcome)
		default:
		}
	})
}
