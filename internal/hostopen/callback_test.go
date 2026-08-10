package hostopen

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCallbackRejectsStateMismatchWithoutConsumingLease(t *testing.T) {
	lease := startTestCallback(t, time.Second)
	defer lease.Close()

	mismatch := callbackGET(t, lease.URL()+"?state=not-the-state&code=do-not-render")
	if mismatch.StatusCode != http.StatusForbidden {
		t.Fatalf("state mismatch status = %d, want %d", mismatch.StatusCode, http.StatusForbidden)
	}
	assertResponseOmits(t, mismatch, "not-the-state", "do-not-render")

	accepted := callbackGET(t, lease.URL()+"?state="+url.QueryEscape(lease.State())+"&code=accepted")
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", accepted.StatusCode, http.StatusOK)
	}
	accepted.Body.Close()
	result, err := lease.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Query.Get("code") != "accepted" || result.Query.Has("state") {
		t.Fatalf("callback query = %#v", result.Query)
	}
}

func TestCallbackRequiresGETAndExactPath(t *testing.T) {
	lease := startTestCallback(t, time.Second)
	defer lease.Close()
	query := "?state=" + url.QueryEscape(lease.State())

	postRequest, err := http.NewRequest(http.MethodPost, lease.URL()+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	post := callbackDo(t, postRequest)
	if post.StatusCode != http.StatusMethodNotAllowed || post.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("POST response = %d Allow %q", post.StatusCode, post.Header.Get("Allow"))
	}
	post.Body.Close()

	wrongPath := callbackGET(t, lease.URL()+"/extra"+query)
	if wrongPath.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-path status = %d, want %d", wrongPath.StatusCode, http.StatusNotFound)
	}
	wrongPath.Body.Close()
	wrongHostRequest, err := http.NewRequest(http.MethodGet, lease.URL()+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongHostRequest.Host = "localhost.invalid"
	wrongHost := callbackDo(t, wrongHostRequest)
	if wrongHost.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-host status = %d, want %d", wrongHost.StatusCode, http.StatusNotFound)
	}
	wrongHost.Body.Close()

	accepted := callbackGET(t, lease.URL()+query)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", accepted.StatusCode, http.StatusOK)
	}
	accepted.Body.Close()
}

func TestCallbackBoundsRequestLineHeadersQueryAndBody(t *testing.T) {
	t.Run("request line", func(t *testing.T) {
		lease := startTestCallback(t, time.Second)
		defer lease.Close()
		response := callbackGET(t, lease.URL()+strings.Repeat("x", maxCallbackRequestLine))
		if response.StatusCode != http.StatusRequestURITooLong {
			t.Fatalf("oversize request-line status = %d, want %d", response.StatusCode, http.StatusRequestURITooLong)
		}
		response.Body.Close()
	})

	t.Run("headers", func(t *testing.T) {
		lease := startTestCallback(t, time.Second)
		defer lease.Close()
		request, err := http.NewRequest(http.MethodGet, lease.URL()+"?state="+url.QueryEscape(lease.State()), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Oversize", strings.Repeat("h", maxCallbackHeaders))
		response := callbackDo(t, request)
		if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversize-header status = %d, want %d", response.StatusCode, http.StatusRequestHeaderFieldsTooLarge)
		}
		response.Body.Close()
	})

	t.Run("query", func(t *testing.T) {
		lease := startTestCallback(t, time.Second)
		defer lease.Close()
		oversize := "?state=" + url.QueryEscape(lease.State()) + "&padding=" + strings.Repeat("q", maxCallbackQuery)
		response := callbackGET(t, lease.URL()+oversize)
		if response.StatusCode != http.StatusRequestURITooLong {
			t.Fatalf("oversize-query status = %d, want %d", response.StatusCode, http.StatusRequestURITooLong)
		}
		response.Body.Close()
	})

	t.Run("body", func(t *testing.T) {
		lease := startTestCallback(t, time.Second)
		defer lease.Close()
		request, err := http.NewRequest(http.MethodGet, lease.URL()+"?state="+url.QueryEscape(lease.State()), strings.NewReader(strings.Repeat("b", maxCallbackBody+1)))
		if err != nil {
			t.Fatal(err)
		}
		response := callbackDo(t, request)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize-body status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
		}
		response.Body.Close()
	})
}

func TestCallbackAcceptsOnlyOneDelivery(t *testing.T) {
	lease := startTestCallback(t, time.Second)
	defer lease.Close()
	callbackURL := lease.URL() + "?state=" + url.QueryEscape(lease.State()) + "&code=first"

	first := callbackGET(t, callbackURL)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.StatusCode, http.StatusOK)
	}
	first.Body.Close()
	duplicate := callbackGET(t, callbackURL)
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want %d", duplicate.StatusCode, http.StatusConflict)
	}
	duplicate.Body.Close()

	result, err := lease.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Query.Get("code"); got != "first" {
		t.Fatalf("accepted code = %q, want first", got)
	}
}

func TestCallbackTimeoutClosesListener(t *testing.T) {
	lease := startTestCallback(t, 30*time.Millisecond)
	_, err := lease.Wait(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want deadline exceeded", err)
	}
	assertListenerClosed(t, lease.URL())
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCallbackCancellationClosesListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := StartCallback(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = lease.Wait(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context canceled", err)
	}
	assertListenerClosed(t, lease.URL())
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCallbackResponsesNeverRenderTokens(t *testing.T) {
	lease := startTestCallback(t, time.Second)
	defer lease.Close()
	const secret = "secret-code-value-that-must-not-render"
	response := callbackGET(t, lease.URL()+"?state="+url.QueryEscape(lease.State())+"&code="+secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	assertResponseOmits(t, response, secret, lease.State())
}

func TestCallbackCloseIsIdempotentAndLeavesNoListener(t *testing.T) {
	lease := startTestCallback(t, time.Second)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	assertListenerClosed(t, lease.URL())
}

func startTestCallback(t *testing.T, duration time.Duration) *CallbackLease {
	t.Helper()
	lease, err := StartCallback(context.Background(), duration)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func callbackGET(t *testing.T, callbackURL string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return callbackDo(t, request)
}

func callbackDo(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	request.Close = true
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func assertResponseOmits(t *testing.T, response *http.Response, secrets ...string) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(body), secret) {
			t.Fatalf("response rendered a callback secret")
		}
	}
}

func assertListenerClosed(t *testing.T, callbackURL string) {
	t.Helper()
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", parsed.Host, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("callback listener accepted a connection after close")
	}
}
