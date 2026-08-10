package main

import "testing"

func TestProductionLoginBrowserOpenerIsConfigured(t *testing.T) {
	opener, err := productionLoginBrowserOpener()
	if err != nil {
		t.Fatal(err)
	}
	if opener == nil {
		t.Fatal("production login browser opener is nil")
	}
}
