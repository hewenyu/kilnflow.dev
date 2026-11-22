package notify

import "testing"

func TestHubAll(t *testing.T) {
	h := NewHub()
	wait := h.JobsChanged()
	h.All()
	select {
	case <-wait:
	default:
		t.Fatal("All 之后 JobsChanged 返回的通道应已关闭")
	}
	for name, ch := range map[string]<-chan struct{}{"outbox": h.Outbox(), "publish": h.Publish()} {
		select {
		case <-ch:
		default:
			t.Fatalf("All 之后 %s 应收到信号", name)
		}
	}
	select {
	case <-h.Ingest():
		t.Fatal("All 不应唤醒 ingest")
	default:
	}
}

func TestKickIngestDoesNotBlock(t *testing.T) {
	h := NewHub()
	h.KickIngest()
	h.KickIngest() // 通道已满时不阻塞
	<-h.Ingest()
	select {
	case <-h.Ingest():
		t.Fatal("多次 kick 只应保留一个信号")
	default:
	}
}
