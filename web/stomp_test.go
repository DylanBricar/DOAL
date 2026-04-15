package web

import (
	"strings"
	"testing"
)

// TestParseFrameConnect verifies parsing of a STOMP CONNECT frame.
func TestParseFrameConnect(t *testing.T) {
	raw := "CONNECT\naccept-version:1.2\n\n\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.command != "CONNECT" {
		t.Errorf("command: want CONNECT, got %q", frame.command)
	}
	if frame.headers["accept-version"] != "1.2" {
		t.Errorf("accept-version: want 1.2, got %q", frame.headers["accept-version"])
	}
	if len(frame.body) != 0 {
		t.Errorf("body: want empty, got %q", frame.body)
	}
}

// TestParseFrameSend verifies parsing of a STOMP SEND frame with no body.
func TestParseFrameSend(t *testing.T) {
	raw := "SEND\ndestination:/doal/global/start\n\n\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.command != "SEND" {
		t.Errorf("command: want SEND, got %q", frame.command)
	}
	if frame.headers["destination"] != "/doal/global/start" {
		t.Errorf("destination: want /doal/global/start, got %q", frame.headers["destination"])
	}
}

// TestParseFrameSubscribe verifies parsing of a STOMP SUBSCRIBE frame.
func TestParseFrameSubscribe(t *testing.T) {
	raw := "SUBSCRIBE\nid:sub-0\ndestination:/global\n\n\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.command != "SUBSCRIBE" {
		t.Errorf("command: want SUBSCRIBE, got %q", frame.command)
	}
	if frame.headers["id"] != "sub-0" {
		t.Errorf("id header: want sub-0, got %q", frame.headers["id"])
	}
	if frame.headers["destination"] != "/global" {
		t.Errorf("destination: want /global, got %q", frame.headers["destination"])
	}
}

// TestParseFrameWithBody verifies that a body after the blank-line separator is captured.
func TestParseFrameWithBody(t *testing.T) {
	body := `{"minUploadRate":100}`
	raw := "SEND\ndestination:/doal/config/save\n\n" + body + "\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if string(frame.body) != body {
		t.Errorf("body: want %q, got %q", body, frame.body)
	}
}

// TestParseFrameCRLF verifies that CRLF line endings are normalised.
func TestParseFrameCRLF(t *testing.T) {
	raw := "CONNECT\r\naccept-version:1.2\r\n\r\n\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.command != "CONNECT" {
		t.Errorf("command: want CONNECT, got %q", frame.command)
	}
	if frame.headers["accept-version"] != "1.2" {
		t.Errorf("accept-version: want 1.2, got %q", frame.headers["accept-version"])
	}
}

// TestParseFrameEmpty verifies that an empty frame returns an error.
func TestParseFrameEmpty(t *testing.T) {
	_, err := parseFrame([]byte("\x00"))
	if err == nil {
		t.Error("expected error for empty frame, got nil")
	}
}

// TestParseFrameMultipleHeaders verifies that all header lines are captured.
func TestParseFrameMultipleHeaders(t *testing.T) {
	raw := "CONNECT\naccept-version:1.2\nheart-beat:0,0\nX-Joal-Auth-Token:secret\n\n\x00"
	frame, err := parseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.headers["heart-beat"] != "0,0" {
		t.Errorf("heart-beat: want 0,0, got %q", frame.headers["heart-beat"])
	}
	if frame.headers["X-Joal-Auth-Token"] != "secret" {
		t.Errorf("X-Joal-Auth-Token: want secret, got %q", frame.headers["X-Joal-Auth-Token"])
	}
}

// TestMarshalFrameCommand verifies that marshalFrame produces the correct command line.
func TestMarshalFrameCommand(t *testing.T) {
	data := marshalFrame("MESSAGE", map[string]string{"destination": "/speed"}, []byte(`{"test":true}`))
	s := string(data)
	if !strings.HasPrefix(s, "MESSAGE\n") {
		t.Errorf("expected MESSAGE\\n prefix, got: %q", s[:min(20, len(s))])
	}
}

// TestMarshalFrameHeader verifies that marshalFrame includes the given header.
func TestMarshalFrameHeader(t *testing.T) {
	data := marshalFrame("MESSAGE", map[string]string{"destination": "/speed"}, []byte(`{}`))
	s := string(data)
	if !strings.Contains(s, "destination:/speed\n") {
		t.Errorf("expected destination:/speed header in: %q", s)
	}
}

// TestMarshalFrameNullTerminator verifies that marshalFrame ends with a null byte.
func TestMarshalFrameNullTerminator(t *testing.T) {
	data := marshalFrame("MESSAGE", map[string]string{}, []byte("body"))
	if len(data) == 0 || data[len(data)-1] != '\x00' {
		t.Error("expected null byte terminator")
	}
}

// TestMarshalFrameBody verifies that the body appears after the blank-line separator.
func TestMarshalFrameBody(t *testing.T) {
	body := `{"hello":"world"}`
	data := marshalFrame("SEND", map[string]string{}, []byte(body))
	s := string(data)
	if !strings.Contains(s, "\n\n"+body) {
		t.Errorf("expected body after blank line in: %q", s)
	}
}

// TestMarshalFrameNoHeaders verifies that a frame with no headers still has the separator.
func TestMarshalFrameNoHeaders(t *testing.T) {
	data := marshalFrame("HEARTBEAT", nil, nil)
	s := string(data)
	if !strings.Contains(s, "\n\n") {
		t.Error("expected blank-line separator even with no headers")
	}
}

// TestParseFrameRoundTrip verifies that marshal followed by parse preserves command and headers.
func TestParseFrameRoundTrip(t *testing.T) {
	orig := map[string]string{
		"destination":    "/announce",
		"content-type":   "application/json",
		"content-length": "2",
	}
	body := []byte("{}")
	marshalled := marshalFrame("MESSAGE", orig, body)

	frame, err := parseFrame(marshalled)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if frame.command != "MESSAGE" {
		t.Errorf("command: want MESSAGE, got %q", frame.command)
	}
	for k, v := range orig {
		if frame.headers[k] != v {
			t.Errorf("header %q: want %q, got %q", k, v, frame.headers[k])
		}
	}
	if string(frame.body) != string(body) {
		t.Errorf("body: want %q, got %q", body, frame.body)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
