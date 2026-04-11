package pipeline

import (
	"fmt"
	"log/slog"
	"sync"

	"rtc-media-server/internal/media"
)

// StageConfig 描述一个可配置的 pipeline stage。
// Enabled 为空时按启用处理，便于配置文件只写 name。
type StageConfig struct {
	Name    string
	Enabled *bool
	Params  map[string]any
}

// Dependencies 是 StageFactory 创建 stage 时可使用的外部依赖。
// 这里保持通用，避免 registry 直接依赖 WebSocket、WebRTC 或业务模块。
type Dependencies struct {
	Logger       *slog.Logger
	TargetFormat media.Format
}

// StageFactory 根据配置创建一个 stage。
type StageFactory func(cfg StageConfig, deps Dependencies) (media.Stage, error)

// Registry 保存可用 stage 工厂，用于按配置组装可重排的 stage chain。
type Registry struct {
	mu        sync.RWMutex
	factories map[string]StageFactory
}

// NewRegistry 创建 stage registry。
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]StageFactory)}
}

// Register 注册一个 stage 工厂。同名注册会覆盖旧工厂。
func (r *Registry) Register(name string, factory StageFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// Build 按配置顺序创建 stage chain。
// 未知 stage 会返回错误，避免启动后静默丢失处理能力。
func (r *Registry) Build(configs []StageConfig, deps Dependencies) ([]media.Stage, error) {
	stages := make([]media.Stage, 0, len(configs))
	for _, cfg := range configs {
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		r.mu.RLock()
		factory := r.factories[cfg.Name]
		r.mu.RUnlock()
		if factory == nil {
			return nil, fmt.Errorf("unknown pipeline stage %q", cfg.Name)
		}
		stage, err := factory(cfg, deps)
		if err != nil {
			return nil, fmt.Errorf("build pipeline stage %q: %w", cfg.Name, err)
		}
		if stage == nil {
			return nil, fmt.Errorf("pipeline stage %q factory returned nil", cfg.Name)
		}
		stages = append(stages, stage)
	}
	return stages, nil
}
