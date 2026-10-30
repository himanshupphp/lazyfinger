package provider

import (
	"context"
	"fmt"
	"time"
)

// Chain tries providers in order and moves to the next one when the current
// one runs out of quota, is rate limited, or can't be reached. Messages are
// provider-neutral, so the next model picks up exactly where the last stopped.
type Chain struct {
	items    []Provider
	cur      int
	state    *State
	OnSwitch func(from, to Provider, reason string) bool // return false to stop
}

func NewChain(items []Provider, st *State) *Chain { return &Chain{items: items, state: st} }

func (c *Chain) Name() string { return c.items[c.cur].Name() }
func (c *Chain) Remote() bool { return c.items[c.cur].Remote() }

// SkipCooling starts from the first provider that isn't paused. If every
// provider is paused, it stays on the first one and tries it anyway.
func (c *Chain) SkipCooling() []string {
	var skipped []string
	for i := range c.items {
		cd, cooling := c.state.Cooling(c.items[i].Name())
		if !cooling {
			c.cur = i
			return skipped
		}
		skipped = append(skipped, fmt.Sprintf("%s (%s, until %s)", c.items[i].Name(), cd.Reason, cd.Until.Local().Format("15:04")))
	}
	c.cur = 0
	return nil
}

func (c *Chain) Complete(ctx context.Context, system string, msgs []Message) (string, error) {
	for {
		p := c.items[c.cur]
		out, err := p.Complete(ctx, system, msgs)
		if err == nil {
			c.state.Clear(p.Name())
			return out, nil
		}
		reason, wait, ok := Classify(err)
		if !ok {
			return "", err
		}
		if wait > 0 {
			c.state.Set(p.Name(), reason, time.Now().Add(wait))
		}
		next := c.nextAvailable()
		if next < 0 {
			return "", fmt.Errorf("%s %s and no other provider is available (add one with `lazy setup`): %w", p.Name(), reason, err)
		}
		if c.OnSwitch != nil && !c.OnSwitch(p, c.items[next], reason) {
			return "", fmt.Errorf("%s %s: %w", p.Name(), reason, err)
		}
		c.cur = next
	}
}

func (c *Chain) nextAvailable() int {
	for i := c.cur + 1; i < len(c.items); i++ {
		if _, cooling := c.state.Cooling(c.items[i].Name()); !cooling {
			return i
		}
	}
	return -1
}
