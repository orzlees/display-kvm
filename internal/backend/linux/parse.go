package linux

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orzlee/displayKvm/internal/model"
)

var (
	displayHeader = regexp.MustCompile(`^Display ([1-9][0-9]*)$`)
	i2cPath       = regexp.MustCompile(`^/dev/i2c-(0|[1-9][0-9]{0,2})$`)
	manufacturer  = regexp.MustCompile(`^[A-Z]{3}$`)
	connectorName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	connectorID   = regexp.MustCompile(`^-?[0-9]+$`)
	inputValue    = regexp.MustCompile(`^x([0-9a-fA-F]{1,4})$`)
)

type displayBlock struct {
	active        bool
	valid         bool
	bus           string
	name          string
	mfg           string
	serial        string
	monitorSeen   bool
	connectorSeen bool
	idSeen        bool
}

// parseDisplays只接受2.2.7-release的简洁枚举字段；该版本除旧文档示例中的
// I2C bus/Monitor外，还输出drm_connector_id及可选DRM connector。
// 显示序号仅用于检测损坏输出，连接器可能因重复EDID被上游误关联，均不用于选屏。
// 总线只标识当前候选，不能作为持久身份；任何异常都丢弃整个结果，避免部分列表误绑定。
func parseDisplays(output CommandOutput) ([]model.Device, error) {
	text := strings.TrimSpace(output.Stdout)
	if text == "No displays found." {
		return []model.Device{}, nil
	}
	if text == "" || !utf8.ValidString(text) || strings.ContainsAny(text, "\x00\x1b") {
		return nil, invalidOutput("枚举显示器", output, nil)
	}
	devices := make([]model.Device, 0)
	seenBuses := make(map[string]bool)
	seenOrdinals := make(map[uint64]bool)
	var block displayBlock
	// 按块完成后才添加候选；非工作状态即使带有有效EDID，也不能获得读写资格。
	finish := func() error {
		if !block.active {
			return nil
		}
		if block.bus != "" {
			if seenBuses[block.bus] {
				return model.NewError(model.CodeIdentityAmbiguous, "ddcutil返回重复I2C总线，不能唯一选择显示器", nil)
			}
			seenBuses[block.bus] = true
		}
		if !block.valid {
			return nil
		}
		if block.bus == "" || !block.monitorSeen || !block.idSeen {
			return invalidOutput("枚举显示器，设备字段缺失", output, nil)
		}
		devices = append(devices, model.Device{
			BackendID: strings.TrimPrefix(block.bus, "/dev/"),
			Name:      block.name,
			Identity: model.IdentityHints{
				Manufacturer: block.mfg,
				Serial:       block.serial,
				Path:         block.bus,
			},
		})
		return nil
	}
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if parts := displayHeader.FindStringSubmatch(line); parts != nil {
			if err := finish(); err != nil {
				return nil, err
			}
			ordinal, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return nil, invalidOutput("枚举显示器，显示序号无效", output, err)
			}
			if seenOrdinals[ordinal] {
				return nil, model.NewError(model.CodeIdentityAmbiguous, "ddcutil返回重复显示序号，不能确认枚举完整性", nil)
			}
			seenOrdinals[ordinal] = true
			block = displayBlock{active: true, valid: true}
			continue
		}
		switch line {
		case "Invalid display", "Busy display", "Removed display", "Phantom display", "DDC_disabled":
			if err := finish(); err != nil {
				return nil, err
			}
			block = displayBlock{active: true}
			continue
		}
		if !block.active {
			return nil, invalidOutput("枚举显示器，字段缺少设备头", output, nil)
		}
		key, value, found := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		if !found {
			return nil, invalidOutput("枚举显示器，未知行", output, nil)
		}
		switch key {
		case "I2C bus":
			parts := i2cPath.FindStringSubmatch(value)
			if parts == nil || block.bus != "" {
				return nil, invalidOutput("枚举显示器，总线格式无效或重复", output, nil)
			}
			bus, err := strconv.ParseUint(parts[1], 10, 8)
			if err != nil {
				return nil, invalidOutput("枚举显示器，总线超出2.2.7支持范围", output, err)
			}
			block.bus = "/dev/i2c-" + strconv.FormatUint(bus, 10)
		case "Monitor":
			parts := strings.SplitN(value, ":", 3)
			if block.monitorSeen || len(parts) != 3 || !manufacturer.MatchString(parts[0]) || strings.TrimSpace(parts[1]) == "" {
				return nil, invalidOutput("枚举显示器，身份字段无效或重复", output, nil)
			}
			block.monitorSeen = true
			block.mfg, block.name, block.serial = parts[0], strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		case "DRM connector":
			if block.connectorSeen || !connectorName.MatchString(value) {
				return nil, invalidOutput("枚举显示器，连接器字段无效或重复", output, nil)
			}
			block.connectorSeen = true
		case "drm_connector_id":
			id, err := strconv.ParseInt(value, 10, 32)
			if block.idSeen || !connectorID.MatchString(value) || err != nil || id < -1 {
				return nil, invalidOutput("枚举显示器，连接器编号无效或重复", output, err)
			}
			block.idSeen = true
		case "Associated non-phantom display":
			if block.valid || value == "" {
				return nil, invalidOutput("枚举显示器，影子设备关联无效", output, nil)
			}
		default:
			return nil, invalidOutput("枚举显示器，未知字段", output, nil)
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return devices, nil
}

func parseInput(output CommandOutput) (uint16, error) {
	text := strings.TrimSpace(output.Stdout)
	if strings.ContainsAny(text, "\r\n") {
		return 0, invalidOutput("读取输入，响应不是单条记录", output, nil)
	}
	fields := strings.Fields(text)
	if len(fields) != 4 || fields[0] != "VCP" || fields[1] != "60" || fields[2] != "SNC" {
		// 2.2.7的ERR合并了不支持与其他通信失败，不能把它解释成硬件明确不支持。
		return 0, invalidOutput("读取输入，缺少有效VCP观测", output, nil)
	}
	parts := inputValue.FindStringSubmatch(fields[3])
	if parts == nil {
		return 0, invalidOutput("读取输入，数值格式无效", output, nil)
	}
	value, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil || value == 0 {
		return 0, invalidOutput("读取输入，数值超出有效输入范围", output, err)
	}
	return uint16(value), nil
}

func invalidOutput(stage string, output CommandOutput, cause error) error {
	if cause == nil {
		cause = errors.New("命令输出不符合已核对的ddcutil 2.2.7格式")
	}
	// 路径、序列号及外部程序原始输出只保留在不序列化的根因中，避免进入公共响应。
	return model.NewError(model.CodeTransport, "ddcutil"+stage+"输出无效，不能据此确认操作结果", fmt.Errorf("stdout=%q stderr=%q: %w", output.Stdout, output.Stderr, cause))
}
