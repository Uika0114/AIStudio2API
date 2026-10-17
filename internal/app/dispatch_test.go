package app

import (
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"testing"
)

func TestDispatchSeparatesStrictChannelsAndAuto(t *testing.T) {
	auto := aistudio.AccountSelection{ModelID: "fixture-model", Method: "generateContent"}
	pg := auto
	pg.Channel = aistudio.ChannelPlayground
	pg.StrictChannel = true
	build := auto
	build.Channel = aistudio.ChannelBuild
	build.StrictChannel = true
	if dispatchKey(auto) == dispatchKey(pg) || dispatchKey(auto) == dispatchKey(build) || dispatchKey(pg) == dispatchKey(build) {
		t.Fatal("different route policies share a queue")
	}
	same := pg
	same.AllowedAccountIDs = []string{"b", "a"}
	pg.AllowedAccountIDs = []string{"a", "b"}
	if dispatchKey(same) != dispatchKey(pg) {
		t.Fatal("equivalent account allowlist changed queue")
	}
	queue := newDispatchQueue()
	first := queue.join(dispatchKey(pg))
	other := queue.join(dispatchKey(build))
	if len(queue.queues) != 2 {
		t.Fatal("channels serialized through one queue")
	}
	queue.leave(first)
	queue.leave(other)
	if len(queue.queues) != 0 {
		t.Fatal("queue leak")
	}
}
