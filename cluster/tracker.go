package cluster

import (
	"strconv"
	"sync"
)

type Tracker struct {
	mu    sync.RWMutex
	stats map[string]string
}

func (t *Tracker) Add(task string) {
}

func (t *Tracker) Remove(task string) {

}

func (t *Tracker) Tasks() []string {
	return nil
}

func (t *Tracker) Task() {

}
func (t *Tracker) Exists() {

}

func (t *Tracker) id(id uint64) {
	strconv.FormatUint(id, 64)
}

type tasks []uint64

func (t tasks) Len() int { return len(t) }

func (t tasks) Swap(i, j int) { t[i], t[j] = t[j], t[i] }

func (t tasks) Less(i, j int) bool { return t[i] < t[j] }
