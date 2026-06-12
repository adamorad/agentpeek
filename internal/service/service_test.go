package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	const bin = "/usr/local/bin/airlock"
	got := LaunchdPlist(bin)

	for _, want := range []string{
		"com.airlock.daemon",
		bin,
		"<string>daemon</string>",
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
		"/tmp/airlock.log",
		"<key>StandardErrorPath</key>",
		"<key>StandardOutPath</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q\n%s", want, got)
		}
	}

	// Well-formedness: the body after the DOCTYPE must parse as XML.
	body := got
	if i := strings.Index(body, "<plist"); i >= 0 {
		body = body[i:]
	}
	var node any
	if err := xml.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("plist is not well-formed XML: %v", err)
	}
}

func TestSystemdUnit(t *testing.T) {
	got := SystemdUnit("/x/airlock")
	for _, want := range []string{
		"ExecStart=/x/airlock daemon",
		"Restart=always",
		"RestartSec=2",
		"WantedBy=default.target",
		"Description=Airlock agent coordination daemon",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q\n%s", want, got)
		}
	}
}
