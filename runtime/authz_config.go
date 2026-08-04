package runtime

import "github.com/BurntSushi/toml"

// LoadAuthzConfig 从指定 TOML 文件加载鉴权配置。
func LoadAuthzConfig(path string) (AuthzConfig, error) {
	var cfg AuthzConfig
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}
