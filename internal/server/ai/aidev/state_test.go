package aidev

import (
	"fmt"
	"testing"
)

// TestTransition 穷举全部（阶段, 状态, 本轮是否执行过任务, 事件）组合（PLAN #26.9、#31.3）：表中没有的组合不改变位置。
func TestTransition(t *testing.T) {
	states := []string{StateDeveloping, StateDelivering, StatePROpen, StateFixing, StateBlocked, StateMerged, StateStopped, StateNoChange}
	events := []Event{
		EvDelivered, EvNoChange, EvNoProgress, EvBlocked, EvExecFailed, EvPromoted, EvPromoteRejected,
		EvChecksFailed, EvRoundsExhausted, EvConflict, EvMerged, EvMergedPartial, EvStop, EvRetry,
	}
	active := []string{StateDeveloping, StateDelivering, StatePROpen, StateFixing, StateBlocked}
	// 实现阶段、本轮执行过任务（#26.9 的表，加上部分完成的合并）
	impl := map[string]map[Event]string{
		StateDeveloping: {
			EvDelivered: StateDelivering, EvNoChange: StateNoChange, EvNoProgress: StateBlocked, EvBlocked: StateBlocked,
			EvExecFailed: StateBlocked,
		},
		StateFixing: {
			EvDelivered: StateDelivering, EvNoChange: StateBlocked, EvNoProgress: StateBlocked, EvBlocked: StateBlocked,
			EvExecFailed: StateBlocked,
		},
		StateDelivering: {EvPromoted: StatePROpen, EvPromoteRejected: StateBlocked},
		StatePROpen:     {EvChecksFailed: StateFixing, EvConflict: StateFixing, EvRoundsExhausted: StateBlocked},
		StateBlocked:    {EvRetry: StateFixing},
	}
	type key struct {
		phase string
		ran   bool
	}
	want := map[key]map[string]map[Event]Pos{}
	for _, phase := range []string{PhaseDesign, PhaseImplement} {
		for _, ran := range []bool{true, false} {
			m := map[string]map[Event]Pos{}
			for _, st := range active {
				m[st] = map[Event]Pos{EvStop: {phase, StateStopped}}
				for ev, next := range impl[st] {
					m[st][ev] = Pos{phase, next}
				}
				if phase == PhaseDesign {
					// 设计 PR 合并：进入实现，开始新的轮次（合并事件的两种都一样）
					m[st][EvMerged] = Pos{PhaseImplement, StateDeveloping}
					m[st][EvMergedPartial] = Pos{PhaseImplement, StateDeveloping}
				} else {
					m[st][EvMerged] = Pos{phase, StateMerged}
					m[st][EvMergedPartial] = Pos{phase, StateBlocked}
				}
			}
			if phase == PhaseDesign {
				m[StateDeveloping][EvNoChange] = Pos{phase, StateBlocked} // 设计阶段必须有设计文档
			}
			if phase == PhaseImplement && !ran {
				m[StateBlocked][EvRetry] = Pos{phase, StateDeveloping} // 部分完成后的新一轮：规划实现任务
			}
			want[key{phase, ran}] = m
		}
	}
	for k, m := range want {
		for _, s := range states {
			for _, ev := range events {
				t.Run(fmt.Sprintf("%s/ran=%v/%s+%s", k.phase, k.ran, s, ev), func(t *testing.T) {
					from := Pos{k.phase, s}
					next, ok := Transition(from, k.ran, ev)
					w, has := m[s][ev]
					if !has {
						if ok || next != from {
							t.Fatalf("= %+v, %v；这个组合不应改变位置", next, ok)
						}
						return
					}
					if !ok || next != w {
						t.Fatalf("= %+v, %v；应为 %+v", next, ok, w)
					}
				})
			}
		}
	}
}

func TestLabelFor(t *testing.T) {
	for s, want := range map[string]string{
		StateDeveloping: LabelClaimed, StateDelivering: LabelClaimed, StatePROpen: LabelPROpen, StateFixing: LabelPROpen,
		StateBlocked: LabelBlocked, StateMerged: "", StateStopped: "", StateNoChange: "",
	} {
		if got := LabelFor(s); got != want {
			t.Errorf("LabelFor(%s) = %q，应为 %q", s, got, want)
		}
		if Active(s) != (want != "") {
			t.Errorf("Active(%s) 与是否有状态 label 不一致", s)
		}
	}
}
