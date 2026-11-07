package windows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/orzlee/displayKvm/internal/model"
)

// 可选接口不扩充标准Transport，已有标准fake和不支持厂商命令的传输无需实现。
// 只暴露已审核的LG输入提交，不允许调用方指定地址、操作码或任意I²C报文。
type lgInputTransport interface {
	checkLGInput(context.Context, PhysicalMonitor) error
	setLGInput(context.Context, PhysicalMonitor, uint16) error
}

type nativeLGAPI interface {
	monitorEDID(path string) ([]byte, error)
	openADL() (adlSession, error)
}

type adlSession interface {
	adapters() ([]adlAdapterInfo, error)
	displays(adapter int32) ([]adlDisplayInfo, error)
	edid(adapter, display int32) ([]byte, error)
	writeLG(adapter, display int32, packet [8]byte) error
	close() error
}

// ADL_Main_Control_*使用进程级全局状态。锁覆盖加载、创建、枚举、发送和析构，
// 不能只锁单次DLL调用，否则另一后端实例可能在本次写入前销毁控制上下文。
var adlControlMu sync.Mutex

func lgUnsupported(message string) error {
	return model.NewError(model.CodeUnsupportedOperation, message, nil)
}

func lgInputPacket(value uint16) ([8]byte, error) {
	if value == 0 {
		return [8]byte{}, model.NewError(model.CodeInvalidRequest, "LG输入值必须为1至65535", nil)
	}
	// 0x50是DDC源地址字节，不是EDID的I²C地址；0x6e是固定目的地址。
	// 高低字节均来自已配置的uint16，不截断为8位；校验覆盖前七字节。
	packet := [8]byte{0x6e, 0x50, 0x84, 0x03, 0xf4, byte(value >> 8), byte(value), 0}
	for _, value := range packet[:7] {
		packet[7] ^= value
	}
	return packet, nil
}

// ADL首块最多返回256字节。仅接受包含全部扩展块且校验正确的EDID；更多扩展块
// 在没有明确块索引语义证据时保守拒绝，不能用前128字节匹配或猜测其余内容。
// 注册表EDID属于Windows实例缓存，可能受覆盖或过期影响，不宣称它是实时读屏结果。
func validatedLGEDID(data []byte) ([]byte, error) {
	if len(data) < 128 || len(data) > 256 || !bytes.Equal(data[:8], []byte{0, 255, 255, 255, 255, 255, 255, 0}) {
		return nil, lgUnsupported("缺少可完整核验的EDID基础块")
	}
	if expected := (int(data[126]) + 1) * 128; expected != len(data) {
		return nil, lgUnsupported("EDID扩展块不完整或超出已核验ADL首块范围")
	}
	for start := 0; start < len(data); start += 128 {
		var checksum byte
		for _, value := range data[start : start+128] {
			checksum += value
		}
		if checksum != 0 {
			return nil, lgUnsupported("EDID块校验失败，不能定位物理输出")
		}
	}
	return append([]byte(nil), data...), nil
}

func lgEDIDHash(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

type lgIdentitySnapshot struct {
	gdi      string
	edid     []byte
	topology map[string]nativeTargetName
}

type lgMonitorRoute struct {
	gdi            string
	source, target nativeDisplayID
}

func lgRouteForTarget(target nativeTargetName) lgMonitorRoute {
	return lgMonitorRoute{gdi: target.gdi, source: target.source, target: target.target}
}

func (t *nativeTransport) lgSnapshot(ctx context.Context, monitor PhysicalMonitor) (lgIdentitySnapshot, error) {
	api, ok := t.api.(nativeLGAPI)
	if !ok {
		return lgIdentitySnapshot{}, lgUnsupported("当前Windows传输没有AMD ADL能力")
	}
	if monitor.ID == "" || monitor.ID != monitor.Identity.Path || monitor.lgEDIDHash == "" {
		if monitor.lgIdentityErr != nil {
			return lgIdentitySnapshot{}, monitor.lgIdentityErr
		}
		return lgIdentitySnapshot{}, lgUnsupported("LG输出缺少路径与完整EDID绑定，需要重新枚举")
	}
	mapping, err := t.displayMapping(ctx)
	if err != nil {
		return lgIdentitySnapshot{}, err
	}
	gdi := ""
	for source, target := range mapping {
		if target.Path == monitor.ID {
			if gdi != "" {
				return lgIdentitySnapshot{}, nativeAmbiguous("LG路径对应多个GDI源")
			}
			if lgRouteForTarget(target) != monitor.lgRoute {
				return lgIdentitySnapshot{}, model.NewError(model.CodeUnreachable, "LG显示目标的GDI或源目标路由已变化", nil)
			}
			gdi = source
		}
	}
	if gdi == "" {
		return lgIdentitySnapshot{}, model.NewError(model.CodeUnreachable, "LG目标已不在活动显示拓扑", nil)
	}
	data, err := api.monitorEDID(monitor.ID)
	if err != nil {
		return lgIdentitySnapshot{}, err
	}
	edid, err := validatedLGEDID(data)
	if err != nil {
		return lgIdentitySnapshot{}, err
	}
	if lgEDIDHash(edid) != monitor.lgEDIDHash {
		return lgIdentitySnapshot{}, model.NewError(model.CodeUnreachable, "LG显示实例EDID在操作期间已变化", nil)
	}
	return lgIdentitySnapshot{gdi: gdi, edid: edid, topology: mapping}, ctx.Err()
}

func sameLGSnapshot(a, b lgIdentitySnapshot) bool {
	if a.gdi != b.gdi || !bytes.Equal(a.edid, b.edid) || len(a.topology) != len(b.topology) {
		return false
	}
	for source, target := range a.topology {
		if b.topology[source] != target {
			return false
		}
	}
	return true
}

type adlTarget struct {
	adapter adlAdapterInfo
	display adlDisplayInfo
}

func adlCString(data []byte) (string, error) {
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return "", nativeInvalid("ADL名称缺少NUL终止符")
	}
	// GDI设备名属于ASCII；拒绝损坏/本地代码页字符，不能在解码替换后造成同名误配。
	for _, value := range data[:end] {
		if value < 0x20 || value > 0x7e {
			return "", nativeInvalid("ADL GDI名称不是有效ASCII")
		}
	}
	return normalizeNativeName(string(data[:end])), nil
}

func resolveADLTarget(ctx context.Context, session adlSession, snapshot lgIdentitySnapshot) (adlTarget, error) {
	adapters, err := session.adapters()
	if err != nil {
		return adlTarget{}, err
	}
	if len(adapters) > maxNativeDisplayItems {
		return adlTarget{}, nativeInvalid("ADL显卡数量超出安全上限")
	}
	var matches []adlAdapterInfo
	for _, adapter := range adapters {
		name, err := adlCString(adapter.DisplayName[:])
		if err != nil {
			return adlTarget{}, err
		}
		if name == snapshot.gdi {
			matches = append(matches, adapter)
		}
	}
	if len(matches) == 0 {
		return adlTarget{}, lgUnsupported("活动GDI源没有对应AMD ADL显卡")
	}
	if len(matches) != 1 {
		return adlTarget{}, nativeAmbiguous("活动GDI源对应多个ADL显卡")
	}
	adapter := matches[0]
	// 固定官方SDK的Sample/Overdrive/Overdrive_Sample.cpp以十进制1002筛选AMD；
	// 该字段不能改用PCI的0x1002，也不能因混合显卡机器装有ADL就跳过厂商检查。
	// 厂商匹配只确认显卡，仍须以下方完整EDID唯一匹配来确认具体物理输出。
	if adapter.Size != 1572 || adapter.Index < 0 || adapter.Vendor != 1002 || adapter.Present != 1 || adapter.Exist != 1 {
		return adlTarget{}, lgUnsupported("目标不是有效且存在的AMD ADL显示源")
	}
	displays, err := session.displays(adapter.Index)
	if err != nil {
		return adlTarget{}, err
	}
	if len(displays) > maxNativeDisplayItems {
		return adlTarget{}, nativeInvalid("ADL输出数量超出安全上限")
	}
	var result adlTarget
	count := 0
	seen := make(map[int32]bool)
	for _, display := range displays {
		if err := ctx.Err(); err != nil {
			return adlTarget{}, err
		}
		if display.Mask&3 != 3 {
			return adlTarget{}, lgUnsupported("ADL输出未提供有效连接与映射状态")
		}
		if display.Value&3 != 3 {
			continue
		}
		id := display.ID
		if display.Value&4 != 0 || id.LogicalAdapter != adapter.Index || id.PhysicalAdapter != adapter.Index || id.LogicalDisplay < 0 || id.PhysicalDisplay < 0 {
			return adlTarget{}, lgUnsupported("ADL跨显卡或非本地输出无法可靠解释")
		}
		if seen[id.LogicalDisplay] {
			return adlTarget{}, nativeAmbiguous("ADL输出标识重复")
		}
		seen[id.LogicalDisplay] = true
		data, err := session.edid(adapter.Index, id.LogicalDisplay)
		if err != nil {
			return adlTarget{}, err
		}
		edid, err := validatedLGEDID(data)
		if err != nil {
			return adlTarget{}, err
		}
		if bytes.Equal(edid, snapshot.edid) {
			result = adlTarget{adapter: adapter, display: display}
			count++
		}
	}
	if count == 0 {
		return adlTarget{}, lgUnsupported("ADL输出EDID与Windows显示实例不匹配")
	}
	if count != 1 {
		return adlTarget{}, nativeAmbiguous("多个ADL输出具有相同EDID，禁止猜测目标")
	}
	return result, ctx.Err()
}

func (t *nativeTransport) checkLGInput(ctx context.Context, monitor PhysicalMonitor) error {
	return t.withLGInput(ctx, monitor, nil)
}

func (t *nativeTransport) setLGInput(ctx context.Context, monitor PhysicalMonitor, value uint16) error {
	packet, err := lgInputPacket(value)
	if err != nil {
		return err
	}
	return t.withLGInput(ctx, monitor, &packet)
}

func (t *nativeTransport) withLGInput(ctx context.Context, monitor PhysicalMonitor, packet *[8]byte) (err error) {
	adlControlMu.Lock()
	defer adlControlMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := t.lgSnapshot(ctx, monitor)
	if err != nil {
		return err
	}
	session, err := t.api.(nativeLGAPI).openADL()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.close()) }()
	first, err := resolveADLTarget(ctx, session, before)
	if err != nil {
		return err
	}
	if packet == nil {
		return ctx.Err()
	}
	// 写前重新枚举Windows和ADL两端，再读Windows拓扑封闭ADL查询期间的竞争窗口。
	// 路径、源/目标ID、EDID或ADL物理标识任一改变即拒绝；绝不按数组位置重选。
	after, err := t.lgSnapshot(ctx, monitor)
	if err != nil {
		return err
	}
	second, err := resolveADLTarget(ctx, session, after)
	if err != nil {
		return err
	}
	final, err := t.lgSnapshot(ctx, monitor)
	if err != nil {
		return err
	}
	if !sameLGSnapshot(before, after) || !sameLGSnapshot(before, final) || first != second {
		return model.NewError(model.CodeUnreachable, "LG写入前显示拓扑或ADL输出身份已变化", nil)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 同步发送不重试，返回失败也可能已提交；析构必须等驱动返回后执行。
	return session.writeLG(first.adapter.Index, first.display.ID.LogicalDisplay, *packet)
}
