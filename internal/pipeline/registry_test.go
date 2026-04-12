package pipeline

import (
	"context"
	"testing"

	"rtc-media-server/internal/media"
)

// TestRegistryBuildKeepsOrderAndSkipsDisabled 验证 registry 按配置顺序构建并跳过禁用 stage。
func TestRegistryBuildKeepsOrderAndSkipsDisabled(t *testing.T) {
	registry := NewRegistry()
	registry.Register("first", testStageFactory("first"))
	registry.Register("second", testStageFactory("second"))

	disabled := false
	stages, err := registry.Build([]StageConfig{
		{Name: "first"},
		{Name: "second", Enabled: &disabled},
		{Name: "second"},
	}, Dependencies{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}
	if stages[0].Name() != "first" || stages[1].Name() != "second" {
		t.Fatalf("stage order = %q, %q", stages[0].Name(), stages[1].Name())
	}
}

// TestRegistryBuildUnknownStage 验证未知 stage 会返回错误。
func TestRegistryBuildUnknownStage(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Build([]StageConfig{{Name: "missing"}}, Dependencies{}); err == nil {
		t.Fatal("Build unknown stage returned nil error")
	}
}

// testStageFactory 创建只返回固定名称 stage 的测试工厂。
func testStageFactory(name string) StageFactory {
	return func(cfg StageConfig, deps Dependencies) (media.Stage, error) {
		return media.NewStageFunc(name, func(ctx context.Context, frame media.Frame) (media.Frame, error) {
			return frame, nil
		}), nil
	}
}
