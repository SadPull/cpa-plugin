package main

import "testing"

// Regression: the host bridges pluginapi.HTTPResponse back with tag-less
// camelCase keys ("StatusCode"), not the snake_case the stream bridge uses.
// A snake_case-only decode made every bridged call look like status 0.
func TestDecodeHostHTTPResultAcceptsBothWireShapes(t *testing.T) {
	resp, err := decodeHostHTTPResult([]byte(`{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"e30="}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("camelCase wire: status = %d", resp.StatusCode)
	}
	if string(resp.Body) != "{}" {
		t.Fatalf("body = %q", resp.Body)
	}
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", resp.Headers)
	}

	resp, err = decodeHostHTTPResult([]byte(`{"status_code":403,"body":"e30="}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("snake_case wire: status = %d", resp.StatusCode)
	}
}
