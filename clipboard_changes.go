package main

import (
	"log"
	"os/exec"
	"sync/atomic"
	"time"
)

// changeWatch tracks clipboard change notifications from a helper process.
// Until a helper is running, or once none can run, every tick reads the
// clipboard.
type changeWatch struct {
	dirty   atomic.Bool // a change notification arrived
	polling atomic.Bool // change notifications are unavailable
}

func (w *changeWatch) Changed() bool {
	return w.polling.Load() || w.dirty.Swap(false)
}

// watchLines runs a command that prints a line on every clipboard change,
// such as "wl-paste --watch". It is restarted if it exits after having run
// for a while, and abandoned for polling if it exits right away.
func (w *changeWatch) watchLines(name string, args ...string) {
	w.dirty.Store(true)
	go func() {
		for {
			started := time.Now()
			cmd := exec.Command(name, args...)
			cmd.SysProcAttr = childProcAttr()
			out, err := cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
			}
			if err == nil {
				buf := make([]byte, 512)
				for {
					if _, err := out.Read(buf); err != nil {
						break
					}
					w.dirty.Store(true)
				}
				err = cmd.Wait()
			}
			if time.Since(started) < 5*time.Second {
				log.Printf("watch: %s unavailable (%v), polling instead", name, err)
				w.polling.Store(true)
				return
			}
			log.Printf("watch: %s exited (%v), restarting", name, err)
			w.dirty.Store(true)
			time.Sleep(time.Second)
		}
	}()
}

// watchOneShot runs a command that exits on the next clipboard change, such
// as clipnotify, in a loop. A failure switches to polling.
func (w *changeWatch) watchOneShot(name string, args ...string) {
	w.dirty.Store(true)
	go func() {
		for {
			cmd := exec.Command(name, args...)
			cmd.SysProcAttr = childProcAttr()
			if err := cmd.Run(); err != nil {
				log.Printf("watch: %s failed (%v), polling instead", name, err)
				w.polling.Store(true)
				return
			}
			w.dirty.Store(true)
		}
	}()
}
