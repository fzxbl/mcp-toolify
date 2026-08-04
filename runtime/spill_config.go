package runtime

import (
	"log"
	"os"
	"sync/atomic"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// defaultMaxResultTokens 是工具返回值自动落盘（spill）的默认阈值，单位为估算 token 数。
const defaultMaxResultTokens = 4000

// SpillConfig 是 spill 行为配置（对应 conf/mcp/mcp.toml 的 [spill] 段）。
type SpillConfig struct {
	// MaxResultTokens 为工具返回值的 token 阈值：超过则自动落盘为 spill 资源。
	// 0 或缺省表示用 defaultMaxResultTokens；负数关闭自动 spill，规范写法为 -1。
	MaxResultTokens int `toml:"max_result_tokens"`
}

// spillFileConfig 对应整个 mcp.toml，只取其中的 [spill] 段（其余段由 AuthzConfig 解析）。
type spillFileConfig struct {
	Spill SpillConfig `toml:"spill"`
}

// LoadSpillConfig 从指定 TOML 文件读取 [spill] 段。
func LoadSpillConfig(path string) (SpillConfig, error) {
	var cfg spillFileConfig
	_, err := toml.DecodeFile(path, &cfg)
	return cfg.Spill, err
}

// spillMaxTokens 是进程内生效的阈值，初始值即默认阈值，读侧无需再做兜底。
var spillMaxTokens atomic.Int64

func init() {
	spillMaxTokens.Store(defaultMaxResultTokens)
}

// SetSpillThreshold 设置自动 spill 的 token 阈值。
// 配置语义：传 0（含配置缺省）归一化为 defaultMaxResultTokens；
// 负数关闭自动 spill，规范写法为 -1，小于 -1 的值视为配置疑似有误，归一化为 -1 并告警。
func SetSpillThreshold(tokens int) {
	switch {
	case tokens == 0:
		tokens = defaultMaxResultTokens
	case tokens < -1:
		log.Printf("[mcp] spill threshold %d looks wrong, treat as -1: auto spill disabled", tokens)
		tokens = -1
	}
	spillMaxTokens.Store(int64(tokens))
}

// spillThreshold 返回当前生效阈值；返回负数表示关闭自动 spill。
func spillThreshold() int {
	return int(spillMaxTokens.Load())
}

// InitSpillConfig 从 mcp.toml 加载 spill 配置并设置阈值。
// 文件不存在（stdio / 单测场景）静默用默认值；其他 stat 失败或解析失败则告警后用默认值——
// 阈值不正确不应导致服务起不来。
func InitSpillConfig(path string) {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[mcp] stat spill config %s failed: %v, use default %d tokens",
				path, err, defaultMaxResultTokens)
		}
		SetSpillThreshold(defaultMaxResultTokens)
		return
	}
	cfg, err := LoadSpillConfig(path)
	if err != nil {
		log.Printf("[mcp] load spill config %s failed: %v, use default %d tokens",
			path, err, defaultMaxResultTokens)
		SetSpillThreshold(defaultMaxResultTokens)
		return
	}
	SetSpillThreshold(cfg.MaxResultTokens)
}

// estimateTokens 估算 data 的 token 数，避免引入 tokenizer 依赖：
// ASCII 字符按 4 字符 1 token 累计（向上取整），非 ASCII 每个 rune 计 1 token。
// 这是偏保守的近似（中文估高），宁可早落盘也不要撑爆上下文。
func estimateTokens(data []byte) int {
	ascii, wide := 0, 0
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			ascii++
			i++
			continue
		}
		_, size := utf8.DecodeRune(data[i:])
		wide++
		i += size
	}
	return (ascii+3)/4 + wide
}
