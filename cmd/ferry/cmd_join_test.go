package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ssh config block join writes forwards nothing from the box, and turns
// off everything ssh could hand the box from this machine.
func TestSSHConfigBlockOneWay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FERRY_HOME", filepath.Join(home, "ferry"))
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host *\n    ForwardAgent yes\n"), 0o600)
	st := &boxState{Contact: "zman-box", User: "friend", HisUser: "av", HisHome: "/home/av"}
	if err := writeSSHConfig(st); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	s := string(b)
	for _, bad := range []string{"RemoteForward", "DynamicForward", "LocalForward"} {
		if strings.Contains(s, bad) {
			t.Errorf("block has %s:\n%s", bad, s)
		}
	}
	for _, want := range []string{"ForwardAgent no", "ForwardX11 no", "SetEnv LEND_HIS_USER=av LEND_HIS_HOME=/home/av", "ProxyCommand"} {
		if !strings.Contains(s, want) {
			t.Errorf("block lacks %q:\n%s", want, s)
		}
	}
	// ssh takes the first value it sees: the block must come before "Host *".
	if strings.Index(s, "ForwardAgent no") > strings.Index(s, "ForwardAgent yes") {
		t.Errorf("block is not first:\n%s", s)
	}
	if blockHasTunnels("zman-box") {
		t.Error("new block reported as having tunnels")
	}
}

// What an older ferry join left is removed by the next join or shell.
func TestRemoveOldWayBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FERRY_HOME", filepath.Join(home, "ferry"))
	name := "ferry-test-box-" + filepath.Base(home) // matches no real process stopPipes could end
	d := boxDir(name)
	os.MkdirAll(d, 0o700)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	for _, f := range []string{"sshd_config", "host_key", "host_key.pub", "authorized_keys", "sshd.log"} {
		os.WriteFile(filepath.Join(d, f), []byte("x"), 0o600)
	}
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	os.WriteFile(ak, []byte("ssh-ed25519 AAAAmine me@laptop\nfrom=\"127.0.0.1\",no-port-forwarding ssh-ed25519 AAAAbox lend-friend-box ferry-box:"+name+"\n"), 0o600)
	old := "# >>> ferry box " + name + "\nHost " + name + "\n    User friend\n    RemoteForward 127.0.0.1:1080\n    RemoteForward 127.0.0.1:2200 127.0.0.1:40000\n# <<< ferry box " + name + "\n\nHost other\n    User x\n"
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(old), 0o600)
	st := &boxState{Contact: name, User: "friend", HisUser: "av", HisHome: home, Mount: filepath.Join(home, name), SSHD: "user", Port: 40000}
	if !blockHasTunnels(name) {
		t.Fatal("old block not recognised")
	}
	removeOldWayBack(newOut(false), st)
	for _, f := range []string{"sshd_config", "host_key", "host_key.pub", "authorized_keys", "sshd.log"} {
		if _, err := os.Stat(filepath.Join(d, f)); err == nil {
			t.Errorf("%s still there", f)
		}
	}
	b, _ := os.ReadFile(ak)
	if strings.Contains(string(b), "ferry-box:") || !strings.Contains(string(b), "me@laptop") {
		t.Errorf("authorized_keys now:\n%s", b)
	}
	b, _ = os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Contains(string(b), "RemoteForward") || !strings.Contains(string(b), "Host other") || !strings.Contains(string(b), "Host "+name) {
		t.Errorf("ssh config now:\n%s", b)
	}
	got, _ := loadBox(name)
	if got == nil || got.SSHD != "" || got.Port != 0 {
		t.Errorf("state now %+v", got)
	}
}

func TestKnownHostMatches(t *testing.T) {
	// made by ssh-keygen -H for host "zman-box"
	hashed := "|1|KDU3LM9WO/QCO9hGcJMiwmSMmEA=|3BFX5erPeGxJvjrinLtsk0+mWAY="
	if !knownHostMatches(hashed, "zman-box") || knownHostMatches(hashed, "zman-box2") {
		t.Error("hashed entry")
	}
	if knownHostMatches("|1|KDU3LM9WO/QCO9hGcJMiwmSMmEA=|AAAAAAAAAAAAAAAAAAAAAAAAAAA=", "zman-box") {
		t.Error("a wrong hash matched")
	}
	for f, want := range map[string]bool{"zman-box": true, "other,zman-box": true, "zman-box2": false, "[zman-box]:2222": false} {
		if knownHostMatches(f, "zman-box") != want {
			t.Errorf("%s: %v", f, !want)
		}
	}
}
