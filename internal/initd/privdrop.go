package initd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DropPrivileges permanently switches the whole process (every thread) to
// uid/gid with no supplementary groups, no capabilities and no_new_privs,
// then verifies the result.
//
// The bounding set is left as is: dropping from it needs CAP_SETPCAP, which
// the container does not grant, and PID 1 never execs after this point.
func DropPrivileges(uid, gid int) error {
	// Go applies these to all threads (AllThreadsSyscall).
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	// Leaving uid 0 clears the permitted and effective capability sets.
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	// Clear the ambient and inheritable sets too, on every thread.
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0); e != 0 {
		return fmt.Errorf("clearing ambient capabilities: %w", e)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	return verifyDropped(uid, gid)
}

// verifyDropped checks /proc/self/status after the drop.
func verifyDropped(uid, gid int) error {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	ids := fmt.Sprintf("%d\t%d\t%d\t%d", uid, uid, uid, uid)
	gids := fmt.Sprintf("%d\t%d\t%d\t%d", gid, gid, gid, gid)
	want := map[string]string{
		"Uid": ids, "Gid": gids, "Groups": "",
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000",
		"CapEff": "0000000000000000", "CapAmb": "0000000000000000",
		"NoNewPrivs": "1",
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		w, check := want[key]
		if !check {
			continue
		}
		if got := strings.TrimSpace(val); got != w {
			return fmt.Errorf("after the drop %s is %q, want %q", key, got, w)
		}
		delete(want, key)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for key := range want {
		return fmt.Errorf("after the drop: %s missing from /proc/self/status", key)
	}
	return nil
}
