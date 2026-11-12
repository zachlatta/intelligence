package resources

import (
	"reflect"
	"testing"
	"time"
)

// diskstatsLine is a /proc/diskstats line: reads, sectors read, writes, sectors written and the
// milliseconds the device was busy (io_ticks).
func diskstatsLine(name string, reads, sectorsRead, writes, sectorsWritten, busyMs int) string {
	return "   8       0 " + name + " " + itoa(reads) + " 0 " + itoa(sectorsRead) + " 0 " + itoa(writes) + " 0 " +
		itoa(sectorsWritten) + " 0 0 " + itoa(busyMs) + " 0 0 0 0 0 0 0\n"
}

func TestParseDiskstatsKeepsWholeDisks(t *testing.T) {
	text := diskstatsLine("nvme0n1", 100, 2000, 50, 4000, 1000) +
		diskstatsLine("nvme0n1p1", 90, 1800, 40, 3000, 900) + // a partition: counted in its disk
		diskstatsLine("dm-0", 90, 1800, 40, 3000, 900) + // a mapper on the disk: counted in it
		diskstatsLine("sda", 10, 100, 5, 200, 50) +
		"   7  0 loop0 1 2 3\n" // too short: skipped
	whole := func(name string) bool { return name == "nvme0n1" || name == "sda" }
	got := parseDiskstats(text, whole)
	want := diskCounters{
		"nvme0n1": {readBytes: 2000 * 512, writeBytes: 4000 * 512, ops: 150, busyMs: 1000, hasBusy: true},
		"sda":     {readBytes: 100 * 512, writeBytes: 200 * 512, ops: 15, busyMs: 50, hasBusy: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diskstats = %+v", got)
	}
}

func TestDiskRates(t *testing.T) {
	prev := diskCounters{
		"a": {readBytes: 1000, writeBytes: 0, ops: 10, busyMs: 1000, hasBusy: true},
		"b": {readBytes: 0, writeBytes: 0, ops: 0, busyMs: 0, hasBusy: true},
	}
	next := diskCounters{
		"a":   {readBytes: 6000, writeBytes: 10000, ops: 60, busyMs: 2000, hasBusy: true}, // busy 1s of 5: 20%
		"b":   {readBytes: 5000, writeBytes: 0, ops: 0, busyMs: 4000, hasBusy: true},      // busy 4s of 5: 80%
		"new": {readBytes: 1 << 40, ops: 1 << 30, busyMs: 1 << 30, hasBusy: true},         // no reading before: not counted
	}
	io := diskRates(prev, next, 5)
	if io == nil {
		t.Fatal("no rates")
	}
	if io.readBps != 2000 || io.writeBps != 2000 || io.iops != 10 || io.busyPct == nil || *io.busyPct != 80 {
		t.Errorf("rates = %+v busy %v", io, io.busyPct)
	}
	// a counter that went back (a reset) and a disk without busy time count nothing for that disk
	back := diskCounters{"a": {readBytes: 10, ops: 1, busyMs: 1, hasBusy: true}, "b": {readBytes: 5500, ops: 5}}
	io = diskRates(next, back, 5)
	if io == nil || io.readBps != 100 || io.iops != 1 || io.busyPct != nil {
		t.Errorf("after a reset = %+v", io)
	}
	if diskRates(nil, next, 5) != nil || diskRates(prev, next, 0) != nil {
		t.Error("rates with no reading before, or no time between")
	}
}

func TestParseMountsKeepsLocalFilesystemsOnce(t *testing.T) {
	text := "/dev/nvme0n1p2 / ext4 rw,relatime 0 0\n" +
		"proc /proc proc rw 0 0\n" +
		"tmpfs /run tmpfs rw 0 0\n" +
		"/dev/nvme0n1p1 /boot/efi vfat rw 0 0\n" +
		"/dev/nvme0n1p2 /var/lib/docker ext4 rw 0 0\n" + // the same filesystem again (a bind mount)
		"/dev/sdb1 /mnt/big\\040disk xfs rw 0 0\n" +
		"server:/export /mnt/nfs nfs4 rw 0 0\n" + // network: never statfs'd (it can hang)
		"tank/home /tank/home zfs rw 0 0\n" +
		"tank/data /tank/data zfs rw 0 0\n" + // the same pool's space again
		"/dev/loop3 /snap/core/1 squashfs ro 0 0\n"
	got := parseMounts(text)
	want := []mount{
		{device: "/dev/nvme0n1p2", path: "/", fsType: "ext4"},
		{device: "/dev/nvme0n1p1", path: "/boot/efi", fsType: "vfat"},
		{device: "/dev/sdb1", path: "/mnt/big disk", fsType: "xfs"},
		{device: "tank/home", path: "/tank/home", fsType: "zfs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts = %+v", got)
	}
}

func TestParseDarwinDf(t *testing.T) {
	text := "Filesystem     1024-blocks      Used Available Capacity  Mounted on\n" +
		"/dev/disk3s1s1   971350180  10000000 500000000     3%    /\n" +
		"devfs                  200       200         0   100%    /dev\n" +
		"/dev/disk3s6     971350180   2000000 500000000     1%    /System/Volumes/VM\n" +
		"/dev/disk3s5     971350180 400000000 500000000    45%    /System/Volumes/Data\n" +
		"/dev/disk5s1     100000000  50000000  50000000    50%    /Volumes/Backup Drive\n"
	got := parseDarwinDf(text)
	want := []Filesystem{
		{Mount: "/System/Volumes/Data", Device: "/dev/disk3s5", Total: 971350180 << 10, Available: 500000000 << 10},
		{Mount: "/Volumes/Backup Drive", Device: "/dev/disk5s1", Total: 100000000 << 10, Available: 50000000 << 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("df = %+v", got)
	}
}

func TestParseDarwinIoreg(t *testing.T) {
	text := `+-o IOBlockStorageDriver  <class IOBlockStorageDriver>
    {
      "Statistics" = {"Operations (Write)"=20,"Bytes (Read)"=3000,"Operations (Read)"=10,"Bytes (Write)"=4000,"Total Time (Read)"=5}
    }
+-o IOBlockStorageDriver  <class IOBlockStorageDriver>
    {
      "Statistics" = {"Operations (Write)"=2,"Bytes (Read)"=300,"Operations (Read)"=1,"Bytes (Write)"=400}
    }
`
	got := parseDarwinIoreg(text)
	want := diskCounters{
		"0": {readBytes: 3000, writeBytes: 4000, ops: 30},
		"1": {readBytes: 300, writeBytes: 400, ops: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ioreg = %+v", got)
	}
}

func TestLinuxSampleReadsDiskIOAndFilesystems(t *testing.T) {
	proc, sys := fakeProc(t), t.TempDir()
	write(t, sys, "block/sda/device/vendor", "x\n")
	write(t, sys, "block/loop0/size", "0\n") // no device: not a physical disk
	write(t, proc, "diskstats", diskstatsLine("sda", 0, 0, 0, 0, 0)+diskstatsLine("sda1", 0, 0, 0, 0, 0)+diskstatsLine("loop0", 0, 0, 0, 0, 0))
	home := t.TempDir()
	write(t, proc, "self/mounts", "/dev/sda1 "+home+" ext4 rw 0 0\nproc /proc proc rw 0 0\n")
	r := &Reader{Proc: proc, Sys: sys, Home: home}
	s := r.Sample(time.Unix(100, 0))
	if s.DiskReadBps != nil || s.DiskBusyPct != nil {
		t.Errorf("first sample has disk rates: they need two")
	}
	if len(s.Filesystems) != 1 || s.Filesystems[0].Mount != home || s.Filesystems[0].Total == 0 || s.Filesystems[0].Type != "ext4" {
		t.Errorf("filesystems = %+v", s.Filesystems)
	}
	// 5 seconds later sda read 10 MiB and wrote 5 MiB in 100 operations, busy 2.5s; loop0's I/O does not count
	write(t, proc, "diskstats", diskstatsLine("sda", 60, 20480, 40, 10240, 2500)+diskstatsLine("sda1", 60, 20480, 40, 10240, 2500)+
		diskstatsLine("loop0", 1000, 1<<20, 0, 0, 5000))
	s = r.Sample(time.Unix(105, 0))
	if s.DiskReadBps == nil || *s.DiskReadBps != 2<<20 || *s.DiskWriteBps != 1<<20 || *s.DiskIOPS != 20 || s.DiskBusyPct == nil || *s.DiskBusyPct != 50 {
		t.Errorf("disk = read %v write %v iops %v busy %v", s.DiskReadBps, s.DiskWriteBps, s.DiskIOPS, s.DiskBusyPct)
	}
}

func TestMinuteKeepsDiskIOAndNewestFilesystems(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	m := time.Unix(60, 0)
	fsOld := []Filesystem{{Mount: "/", Total: 100, Available: 50}}
	fsNew := []Filesystem{{Mount: "/", Total: 100, Available: 40}}
	got := aggregate(m, []Sample{
		{At: m, DiskReadBps: f(100), DiskWriteBps: f(10), DiskIOPS: f(4), DiskBusyPct: f(20), Filesystems: fsOld},
		{At: m.Add(5 * time.Second), DiskReadBps: f(300), DiskWriteBps: f(30), DiskIOPS: f(8), DiskBusyPct: f(60), Filesystems: fsNew},
		{At: m.Add(10 * time.Second)}, // filesystems are read once a minute; disk rates unknown
	}, Sample{})
	if got.DiskReadBps == nil || *got.DiskReadBps != 200 || *got.DiskWriteBps != 20 || *got.DiskIOPS != 6 ||
		*got.DiskBusyAvgPct != 40 || *got.DiskBusyMaxPct != 60 || got.DiskReadBpsMax == nil || *got.DiskReadBpsMax != 300 || *got.DiskWriteBpsMax != 30 {
		t.Errorf("minute disk = %+v", got)
	}
	if !reflect.DeepEqual(got.Filesystems, fsNew) {
		t.Errorf("filesystems = %+v", got.Filesystems)
	}
}
