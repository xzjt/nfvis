package compute

import (
	"strings"
	"testing"
)

func TestSerialPtyPath(t *testing.T) {
	xml := `<domain type='kvm'><devices>
    <serial type='pty'>
      <source path='/dev/pts/3'/>
      <target type='isa-serial' port='0'/>
    </serial>
    <console type='pty' tty='/dev/pts/3'>
      <source path='/dev/pts/3'/>
    </console>
  </devices></domain>`
	got, err := SerialPtyPath(xml)
	if err != nil || got != "/dev/pts/3" {
		t.Fatalf("应解析出 /dev/pts/3: %q err=%v", got, err)
	}
	if _, err := SerialPtyPath(`<domain><devices></devices></domain>`); err == nil {
		t.Fatal("无串口应报错")
	}
	if _, err := SerialPtyPath(`<domain><devices><serial type='pty'><target port='0'/></serial></devices></domain>`); err == nil {
		t.Fatal("串口无 pty 源应报错")
	}
	if _, err := SerialPtyPath(""); err == nil || !strings.Contains(err.Error(), "串口") {
		t.Fatalf("空 XML 应报清晰错误: %v", err)
	}
}
