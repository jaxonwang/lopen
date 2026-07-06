package setup

import "testing"

func TestDeriveLabel(t *testing.T) {
	cases := map[string]string{
		"dev-dsk-me-2b-1de9.us-west-2.amazon.com": "dev-dsk-me-2b-1de9",
		"me@jump.example.com":                     "jump",
		"host":                                    "host",
		"user@10.0.0.5":                           "10",
		"":                                        "host",
	}
	for in, want := range cases {
		if got := deriveLabel(in); got != want {
			t.Errorf("deriveLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteArch(t *testing.T) {
	ok := map[string]string{
		"x86_64\n": "amd64",
		"amd64":    "amd64",
		"aarch64":  "arm64",
		"arm64\n":  "arm64",
	}
	for in, want := range ok {
		got, err := remoteArch(in)
		if err != nil || got != want {
			t.Errorf("remoteArch(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := remoteArch("mips"); err == nil {
		t.Error("expected error for unsupported arch")
	}
}
