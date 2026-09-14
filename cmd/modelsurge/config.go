package main

import (
	"fmt"
	"log"
	"os"

	agentconfig "github.com/aceaura/ModelSurge/agent/config"
	replayconfig "github.com/aceaura/ModelSurge/replay/config"
	"github.com/aceaura/ModelSurge/upstream/processconfig"
	"gopkg.in/yaml.v3"
)

// Config 模式一单份 YAML：agent / replay / upstream 三 section，
// 字段与三进程各自配置完全一致（共用各自模块的 Config 类型）。
type Config struct {
	Agent    agentconfig.Config     `yaml:"agent"`
	Replay   replayconfig.Config    `yaml:"replay"`
	Upstream processconfig.Upstream `yaml:"upstream"`
}

// LoadConfig 解析单份 YAML，强制覆写回环互联地址后统一 Normalize。
// replay/upstream 的 listen 与 URL 出现在 YAML 中即告警忽略：
// 单进程内三服务必须走本机回环，且与三进程形态共用同一代码路径。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("modelsurge: read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), &c); err != nil {
		return nil, fmt.Errorf("modelsurge: parse %s: %w", path, err)
	}

	override := func(section, field, old, want string) string {
		if old != "" && old != want {
			log.Printf("modelsurge: %s.%s=%q ignored, forced to %q (loopback wiring)", section, field, old, want)
		}
		return want
	}
	c.Upstream.Listen = override("upstream", "listen", c.Upstream.Listen, upstreamLoopback)
	c.Replay.Listen = override("replay", "listen", c.Replay.Listen, replayLoopback)
	c.Replay.UpstreamURL = override("replay", "upstream_url", c.Replay.UpstreamURL, "http://"+upstreamLoopback)
	c.Agent.ReplayURL = override("agent", "replay_url", c.Agent.ReplayURL, "http://"+replayLoopback)

	// 模式一固定 SQLite：集群数据源（postgres/Redis）属模式二（三进程形态）。
	for _, s := range []struct {
		section string
		driver  *string
		dsn     *string
	}{
		{"agent", &c.Agent.DBDriver, &c.Agent.DBDSN},
		{"replay", &c.Replay.DBDriver, &c.Replay.DBDSN},
		{"upstream", &c.Upstream.DBDriver, &c.Upstream.DBDSN},
	} {
		if *s.driver != "" && *s.driver != "sqlite" {
			log.Printf("modelsurge: %s.db_driver=%q ignored, forced to sqlite (single-process mode)", s.section, *s.driver)
		}
		if *s.dsn != "" {
			log.Printf("modelsurge: %s.db_dsn ignored, using db_path (single-process mode)", s.section)
		}
		*s.driver = ""
		*s.dsn = ""
	}

	if err := c.Upstream.Normalize(); err != nil {
		return nil, fmt.Errorf("modelsurge: upstream section: %w", err)
	}
	if err := c.Replay.Normalize(); err != nil {
		return nil, fmt.Errorf("modelsurge: replay section: %w", err)
	}
	if err := c.Agent.Normalize(); err != nil {
		return nil, fmt.Errorf("modelsurge: agent section: %w", err)
	}
	return &c, nil
}
