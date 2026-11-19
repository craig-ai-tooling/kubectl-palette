package shell

import (
	"os"
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

func TestHookEvalsUnset(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	// The fake plugin prints what the real one prints for `use --unset --print-env`, and
	// records its arguments so the test can see the hook passed the flag through.
	argsFile := dir + "/args"
	if err := writeExec(dir+"/kubectl-palette", "#!/bin/sh\necho \"$*\" > "+argsFile+"\necho '"+UnsetKubeconfig+"'\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeExec(dir+"/kubectl", "#!/bin/sh\necho \"real kubectl kc=[$KUBECONFIG]\"\n"); err != nil {
		t.Fatal(err)
	}
	hookSrc, _ := Init("bash")
	cmd := exec.Command(bash, "-c", hookSrc+"\nexport KUBECONFIG=/x/pinned.yaml\nkubectl palette use --unset\nkubectl get ns\n")
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "real kubectl kc=[]") {
		t.Fatalf("KUBECONFIG was not unset: %s", out)
	}
	got, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(got)) != "use --unset --print-env" {
		t.Fatalf("plugin arguments: %q", got)
	}
}

// TestHookWithKubectlAlias loads the hook from an interactive shell that already has a kubectl
// alias, the way an rc file does. zsh used to fail with "defining function based on alias" and
// a parse error, and bash with a syntax error when the alias had arguments.
func TestHookWithKubectlAlias(t *testing.T) {
	cases := []struct {
		name, alias, want string
	}{
		{"none", "", "real kubectl get ns"},
		{"other command", "alias kubectl=kubecolor", "kubecolor|real kubectl get ns"},
		{"kubectl with flags", "alias kubectl='kubectl --request-timeout=5s'", "real kubectl --request-timeout=5s get ns"},
		{"env prefix", "alias kubectl='KP_T=1 kubectl'", "real kubectl get ns t=1"},
		{"backslash", `alias kubectl='\kubectl -v1'`, "real kubectl -v1 get ns"},
		{"env command", "alias kubectl='env KP_T=1 kubectl'", "real kubectl get ns t=1"},
	}
	for _, sh := range []string{"bash", "zsh"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed, skipped", sh)
			continue
		}
		for _, c := range cases {
			for _, twice := range []bool{false, true} {
				dir := t.TempDir()
				write := func(name, body string) {
					if err := writeExec(dir+"/"+name, body); err != nil {
						t.Fatal(err)
					}
				}
				write("kubectl-palette", "#!/bin/sh\necho \"export KUBECONFIG=/x/c.yaml\"\n")
				write("kubectl", "#!/bin/sh\necho \"real kubectl $*${KP_T:+ t=$KP_T}\"\n")
				write("kubecolor", "#!/bin/sh\nprintf 'kubecolor|'\nexec kubectl \"$@\"\n")
				hookSrc, _ := Init(sh)
				if err := os.WriteFile(dir+"/hook", []byte(hookSrc), 0o600); err != nil {
					t.Fatal(err)
				}
				load := "eval \"$(cat " + dir + "/hook)\"\n"
				if twice {
					load += load
				}
				// The alias and the hook are read in one source step, as from an rc file, and
				// the commands run afterwards, as typed at the prompt.
				rc := c.alias + "\n" + load
				if err := os.WriteFile(dir+"/rc", []byte(rc), 0o600); err != nil {
					t.Fatal(err)
				}
				script := "source " + dir + "/rc\nkubectl palette use foo\necho \"KC=$KUBECONFIG\"\nkubectl get ns\n"
				args := []string{"-c", script}
				if sh == "bash" {
					args = []string{"--norc", "--noprofile", "-c", "shopt -s expand_aliases\n" + script}
				} else {
					args = []string{"-f", "-c", script}
				}
				cmd := exec.Command(bin, args...)
				cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir}
				out, err := cmd.CombinedOutput()
				label := sh + "/" + c.name
				if twice {
					label += "/twice"
				}
				if err != nil {
					t.Errorf("%s: %v: %s", label, err, out)
					continue
				}
				s := string(out)
				if !strings.Contains(s, "KC=/x/c.yaml") || !strings.Contains(s, c.want) {
					t.Errorf("%s: want KC=/x/c.yaml and %q in:\n%s", label, c.want, s)
				}
				if strings.Contains(s, "parse error") || strings.Contains(s, "syntax error") || strings.Contains(s, "based on alias") {
					t.Errorf("%s: shell error:\n%s", label, s)
				}
			}
		}
	}
}
