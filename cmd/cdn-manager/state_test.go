package main

import "testing"

func TestStateDirectorySingleWriter(t *testing.T) {
	dir := t.TempDir()
	first, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockState(dir); err == nil {
		second.Close()
		t.Error("a second manager acquired an already locked directory")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
