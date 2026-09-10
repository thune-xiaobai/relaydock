package store

import "testing"

func TestLockAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("two processes can own the same journal")
	}
	if e = s.Put("calls", "one", map[string]string{"status": "accepted"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var record map[string]string
	if e = s.Get("calls", "one", &record); e != nil || record["status"] != "accepted" {
		t.Fatal(record, e)
	}
}
