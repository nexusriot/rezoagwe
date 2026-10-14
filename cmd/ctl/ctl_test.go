package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
	"time"
)

// newFlags mirrors the flags run() defines, so the parsing tests exercise the
// real shapes rather than a simplified stand-in.
func newFlags() (*flag.FlagSet, *time.Duration, *bool) {
	fs := flag.NewFlagSet("rezoagwectl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ttl := fs.Duration("ttl", 0, "")
	divergent := fs.Bool("divergent", false, "")
	fs.String("server", "", "")
	fs.String("if-match", "", "")
	fs.Bool("json", false, "")
	return fs, ttl, divergent
}

// Flags belong wherever the person typing them expects, which is not only
// before the command. Each of these used to leave the flag in the argument
// list as another key, silently.
func TestFlagsAreAcceptedAnywhere(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantVerb string
		wantArgs []string
		wantTTL  time.Duration
	}{
		{"before the command", []string{"-ttl", "60s", "put", "k", "v"}, "put", []string{"k", "v"}, time.Minute},
		{"after the command", []string{"put", "-ttl", "60s", "k", "v"}, "put", []string{"k", "v"}, time.Minute},
		{"after the positionals", []string{"put", "k", "v", "-ttl", "60s"}, "put", []string{"k", "v"}, time.Minute},
		{"between the positionals", []string{"put", "k", "-ttl", "60s", "v"}, "put", []string{"k", "v"}, time.Minute},
		{"no flags at all", []string{"get", "k"}, "get", []string{"k"}, 0},
		{"command alone", []string{"peers"}, "peers", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, ttl, _ := newFlags()
			verb, args, err := splitArgs(fs, c.argv)
			if err != nil {
				t.Fatalf("splitArgs: %v", err)
			}
			if verb != c.wantVerb {
				t.Errorf("verb = %q, want %q", verb, c.wantVerb)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %q, want %q", args, c.wantArgs)
			}
			if *ttl != c.wantTTL {
				t.Errorf("ttl = %s, want %s", *ttl, c.wantTTL)
			}
		})
	}
}

func TestABooleanFlagAfterTheCommand(t *testing.T) {
	fs, _, divergent := newFlags()
	verb, args, err := splitArgs(fs, []string{"repair", "-divergent"})
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	if verb != "repair" {
		t.Errorf("verb = %q, want repair", verb)
	}
	if len(args) != 0 {
		t.Errorf("args = %q, want none — the flag is not a key to repair", args)
	}
	if !*divergent {
		t.Error("-divergent was not applied")
	}
}

// Keys are arbitrary strings, so one can begin with a dash. "--" is the
// standard escape and the flag package already honours it.
func TestADashLeadingPositionalAfterTheTerminator(t *testing.T) {
	fs, _, _ := newFlags()
	verb, args, err := splitArgs(fs, []string{"put", "--", "-dashed", "value"})
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	if verb != "put" {
		t.Errorf("verb = %q, want put", verb)
	}
	if !reflect.DeepEqual(args, []string{"-dashed", "value"}) {
		t.Errorf("args = %q, want the dashed key and its value", args)
	}
}

// A typo in a flag name has to be an error. Taking it as a key is how a
// mistyped -ttl silently writes a key called "-tl".
func TestAnUnknownFlagIsAnError(t *testing.T) {
	fs, _, _ := newFlags()
	if _, _, err := splitArgs(fs, []string{"put", "k", "v", "-nonsense"}); err == nil {
		t.Error("an unknown flag was accepted as a positional")
	}
}

func TestNoArgumentsYieldsNoCommand(t *testing.T) {
	fs, _, _ := newFlags()
	verb, args, err := splitArgs(fs, nil)
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	if verb != "" || len(args) != 0 {
		t.Errorf("got %q/%q, want nothing", verb, args)
	}
}

// The exit codes are the contract a script branches on, so they have to stay
// distinct and stay what the usage text says they are.
func TestExitCodesAreDistinct(t *testing.T) {
	codes := map[string]int{
		"ok":        exitOK,
		"error":     exitError,
		"not found": exitNotFound,
		"conflict":  exitConflict,
		"divergent": exitDivergent,
	}
	seen := make(map[int]string, len(codes))
	for name, code := range codes {
		if other, dup := seen[code]; dup {
			t.Errorf("%q and %q share exit code %d", name, other, code)
		}
		seen[code] = name
	}
}
