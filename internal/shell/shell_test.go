package shell

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"a b": `'a b'`, "it's": `'it'\''s'`, "": "''"} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestInit(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		out, err := Init(sh)
		if err != nil || !strings.Contains(out, "kubectl-palette use") {
			t.Fatalf("%s: %v", sh, err)
		}
	}
	if _, err := Init("fish"); err == nil {
		t.Fatal("fish should be rejected")
	}
}

func TestHookEvalsExport(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := writeExec(dir+"/"+name, body); err != nil {
			t.Fatal(err)
		}
	}
	write("kubectl-palette", "#!/bin/sh\necho \"export KUBECONFIG='/x/it'\\\\''s.yaml'\"\n")
	write("kubectl", "#!/bin/sh\necho \"real kubectl $* kc=$KUBECONFIG\"\n")
	hookSrc, _ := Init("bash")
	cmd := exec.Command(bash, "-c", hookSrc+"\nkubectl palette use foo\nkubectl get ns\n")
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "real kubectl get ns kc=/x/it's.yaml") {
		t.Fatalf("output: %s", out)
	}
}
