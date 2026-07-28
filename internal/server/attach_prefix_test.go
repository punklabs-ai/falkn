package server

import "testing"

func TestAttachPrefixDefaultsToControlBackslash(t *testing.T) {
	prefix, err := parseAttachPrefix("")
	if err != nil {
		t.Fatal(err)
	}
	if prefix != 0x1c {
		t.Fatalf("default prefix = %#x, want 0x1c", prefix)
	}
	if name := AttachPrefixName(prefix); name != `Ctrl-\` {
		t.Fatalf("prefix name = %q", name)
	}
}

func TestAttachPrefixAcceptsCommonSpellings(t *testing.T) {
	for _, testCase := range []struct {
		value    string
		expected byte
	}{
		{"ctrl-a", 0x01},
		{"CTRL+A", 0x01},
		{"^a", 0x01},
		{"a", 0x01},
		{`ctrl-\`, 0x1c},
		{"ctrl-^", 0x1e},
		{"  ctrl-b  ", 0x02},
	} {
		prefix, err := parseAttachPrefix(testCase.value)
		if err != nil {
			t.Fatalf("%q: %v", testCase.value, err)
		}
		if prefix != testCase.expected {
			t.Fatalf("%q = %#x, want %#x", testCase.value, prefix, testCase.expected)
		}
	}
}

func TestAttachPrefixRejectsUnusableKeys(t *testing.T) {
	for _, value := range []string{"ctrl-c", "ctrl-d", "ctrl-z", "f12", "ctrl-", "esc", "ctrl-1"} {
		if prefix, err := parseAttachPrefix(value); err == nil {
			t.Fatalf("%q was accepted as %#x", value, prefix)
		}
	}
}
