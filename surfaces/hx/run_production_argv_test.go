package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Eastsidegunn/JANUS/core/world/worldtest"
	"github.com/Eastsidegunn/JANUS/seams/store/sqlite"
	"github.com/Eastsidegunn/JANUS/seams/subagent/claudecode"
)

func TestWorldLauncherContainerArgvBeforePrepare(t *testing.T) {
	for _, id := range []string{"claudecode", "codex", "worldadapter"} {
		t.Run(id, func(t *testing.T) {
			cfg := validWorldConfig()
			adapter := cfg.Adapters["claudecode"]
			adapter.AgentArgv = []string{"/opt/agent", "existing-argument"}
			cfg.Adapters[id] = adapter
			backend := worldtest.NewFakeBackend(nil)
			sentinel := errors.New("fake prepare boundary")
			backend.FakeSetPrepareError(sentinel)
			launcher := worldLauncher{backend: backend, config: cfg}
			log, err := sqlite.Open(context.Background(), t.TempDir()+"/events.db")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			in := sessionLaunch{Log: log}
			in.Request.AdapterID = id
			in.Request.TaskRef.Instruction = "ls /workspace\n한글 'quotes'"
			_, err = launcher.Launch(context.Background(), in)
			if !errors.Is(err, sentinel) {
				t.Fatalf("Launch: %v", err)
			}
			specs := backend.FakePreparedSpecs()
			if len(specs) != 1 {
				t.Fatalf("prepares=%d", len(specs))
			}
			want := adapter.AgentArgv
			if id == "claudecode" {
				want = claudecode.ContainerArgv(adapter.AgentArgv[0], in.Request.TaskRef.Instruction)
			}
			if !reflect.DeepEqual(specs[0].AgentArgv(), want) {
				t.Fatalf("argv=%q want=%q", specs[0].AgentArgv(), want)
			}
			if !reflect.DeepEqual(cfg.Adapters[id].AgentArgv, []string{"/opt/agent", "existing-argument"}) {
				t.Fatal("config argv mutated")
			}
		})
	}
}
