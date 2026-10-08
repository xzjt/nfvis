package netkernel

// 跨包一致性守护：internal/model 的派生名撞名校验复刻了本包的命名规则（model 不能 import 编排层，
// 见 model/validate.go 的 kernelDerivedLinkName / kernelDerivedGatewayVRFName）。两份规则一旦漂移，
// 提交期就会漏报撞名（撞名放行 ⇒ 后下发者失败/配置与数据面错位）或误报（合法配置被拒）。
//
// 这里用**本包的真映射**（LinkName / GatewayVRFName）造出"必撞 / 必不撞"两组配置，交叉核对
// model.Validate 的判定：判定与映射一致才算通过，规则漂移即失败。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// kernelValidateErrText 把校验错误拼成便于断言的文本。
func kernelValidateErrText(errs []model.ValidateError) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Path)
		b.WriteString("|")
		b.WriteString(e.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// kernelSwitchWithGateway 一台带网关的 L2 交换机（会派生网关 VRF 设备）。
func kernelSwitchWithGateway(name string) model.VirtualSwitch {
	return model.VirtualSwitch{Name: name, Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}}
}

func TestModelKernelNameCollisionMatchesLinkNameMapping(t *testing.T) {
	// ① 短名形态：`vr-<交换机名>` 不超长，派生名就是它本身。
	short := "lan"
	if got := GatewayVRFName(short); got != "vr-lan" {
		t.Fatalf("前提不成立：GatewayVRFName(%q) = %q（短名不应哈希）", short, got)
	}
	mkShort := func(mode, other string) model.Config {
		return model.Config{
			System:          &model.SystemConfig{DataPlane: mode},
			VirtualSwitches: []model.VirtualSwitch{kernelSwitchWithGateway(short), {Name: other, Type: "l2"}},
		}
	}
	errs := model.Validate(mkShort(model.DataPlaneKernel, GatewayVRFName(short)))
	if len(errs) == 0 {
		t.Fatalf("本包映射下 %q 的网关 VRF 名与同名交换机的设备名相同，model 校验应报撞名",
			short)
	}
	if txt := kernelValidateErrText(errs); !strings.Contains(txt, GatewayVRFName(short)) {
		t.Fatalf("报错应出现**本包算出**的派生设备名 %q（两侧规则一致的可观测判据），得到：\n%s",
			GatewayVRFName(short), txt)
	}
	// 不撞的短名（派生名不同）必须放行——防"判据过宽/规则漂移导致的误报"。
	if errs := model.Validate(mkShort(model.DataPlaneKernel, "vr-other")); len(errs) != 0 {
		t.Fatalf("派生名与对象名不同不应报撞名，得到：\n%s", kernelValidateErrText(errs))
	}
	// VPP 数据面下没有内核设备名这回事：同名对象照常共存。
	if errs := model.Validate(mkShort(model.DataPlaneVPP, GatewayVRFName(short))); len(errs) != 0 {
		t.Fatalf("VPP 数据面不应受派生设备名撞名限制，得到：\n%s", kernelValidateErrText(errs))
	}

	// ② 长名形态：`vr-` + 15 字符名超过上限 ⇒ 本包映射走「前缀 + 8 位哈希」截断，
	// 截断结果是合法对象名 ⇒ 该名字的另一个交换机与派生设备名撞名（离线可构造）。
	long := "abcdefghijklmno"
	derived := GatewayVRFName(long)
	if derived == "vr-"+long || len(derived) > ifnameMax {
		t.Fatalf("前提不成立：GatewayVRFName(%q) = %q 应走哈希截断且 ≤%d 字符", long, derived, ifnameMax)
	}
	mkLong := func(other string) model.Config {
		return model.Config{
			System:          &model.SystemConfig{DataPlane: model.DataPlaneKernel},
			VirtualSwitches: []model.VirtualSwitch{kernelSwitchWithGateway(long), {Name: other, Type: "l2"}},
		}
	}
	errs = model.Validate(mkLong(derived))
	if len(errs) == 0 {
		t.Fatalf("本包映射下 %q 的网关 VRF 名是 %q（哈希截断），同名交换机应被 model 校验拦下",
			long, derived)
	}
	if txt := kernelValidateErrText(errs); !strings.Contains(txt, derived) {
		t.Fatalf("报错应出现**本包算出**的哈希派生名 %q，得到：\n%s", derived, txt)
	}
	// 同一前缀、哈希不同的长名（本包算出各自成名的名字）必须放行。
	other := GatewayVRFName("abcdefghijklmnp")
	if other == derived {
		t.Fatalf("前提不成立：两个不同长名不该映射到同一设备名（%q）", other)
	}
	if errs := model.Validate(mkLong(other)); len(errs) != 0 {
		t.Fatalf("映射到不同设备名的长名不应报撞名，得到：\n%s", kernelValidateErrText(errs))
	}
}
