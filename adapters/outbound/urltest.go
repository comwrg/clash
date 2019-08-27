package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dreamacro/clash/common/picker"
	C "github.com/Dreamacro/clash/constant"
)

type Prefer int32

const (
	Fast Prefer = iota
	Stable
)

type URLTest struct {
	*Base
	proxies  []C.Proxy
	rawURL   string
	fast     C.Proxy
	interval time.Duration
	done     chan struct{}
	once     int32
	prefer   Prefer
}

type URLTestOption struct {
	Name     string   `proxy:"name"`
	Proxies  []string `proxy:"proxies"`
	URL      string   `proxy:"url"`
	Interval int      `proxy:"interval"`
	Prefer   string   `proxy:"prefer,omitempty"`
}

func (u *URLTest) Now() string {
	return u.fast.Name()
}

func (u *URLTest) Dial(metadata *C.Metadata) (C.Conn, error) {
	a, err := u.fast.Dial(metadata)
	if err != nil {
		u.fallback()
	} else {
		a.AppendToChains(u)
	}
	return a, err
}

func (u *URLTest) DialUDP(metadata *C.Metadata) (C.PacketConn, net.Addr, error) {
	pc, addr, err := u.fast.DialUDP(metadata)
	if err == nil {
		pc.AppendToChains(u)
	}
	return pc, addr, err
}

func (u *URLTest) SupportUDP() bool {
	return u.fast.SupportUDP()
}

func (u *URLTest) MarshalJSON() ([]byte, error) {
	var all []string
	for _, proxy := range u.proxies {
		all = append(all, proxy.Name())
	}
	return json.Marshal(map[string]interface{}{
		"type": u.Type().String(),
		"now":  u.Now(),
		"all":  all,
	})
}

func (u *URLTest) Destroy() {
	u.done <- struct{}{}
}

func (u *URLTest) loop() {
	tick := time.NewTicker(u.interval)
	go u.speedTest()
Loop:
	for {
		select {
		case <-tick.C:
			go u.speedTest()
		case <-u.done:
			break Loop
		}
	}
}

func (u *URLTest) fallback() {
	Inf := 0xffff
	fast := u.fast
	if fast == nil {
		fast = u.proxies[0]
	}
	prefer := u.prefer

	switch len(fast.DelayHistory()) {
	case 0:
		return
	case 1:
		// only one history, `stable` is no meaning
		prefer = Fast
	}

	minDelay := Inf
	minInvalid := Inf
	for _, proxy := range u.proxies {
		if !proxy.Alive() {
			continue
		}

		switch prefer {
		case Fast:
			delay := int(proxy.LastDelay())
			if delay < minDelay {
				fast = proxy
				minDelay = delay
			}
		case Stable:
			// calculate variance
			sum := 0.0
			invalidNum := 0
			for _, e := range proxy.DelayHistory() {
				if e.Delay == 0 {
					invalidNum++
				} else {
					sum += float64(e.Delay)
				}
			}
			totalNum := len(proxy.DelayHistory())
			validNum := totalNum - invalidNum
			mean := sum / float64(validNum)

			sum = 0.0
			for _, e := range proxy.DelayHistory() {
				if e.Delay == 0 {
					continue
				}
				sum += math.Pow(float64(e.Delay)-mean, 2)
			}

			delay := int(sum / float64(validNum))

			if invalidNum <= minInvalid && delay < minDelay {
				fast = proxy
				minInvalid = invalidNum
				minDelay = delay
			}
		}
	}
	u.fast = fast
}

func (u *URLTest) speedTest() {
	if !atomic.CompareAndSwapInt32(&u.once, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&u.once, 0)

	picker, ctx, cancel := picker.WithTimeout(context.Background(), defaultURLTestTimeout)
	defer cancel()
	for _, p := range u.proxies {
		proxy := p
		picker.Go(func() (interface{}, error) {
			_, err := proxy.URLTest(ctx, u.rawURL)
			if err != nil {
				return nil, err
			}
			return proxy, nil
		})
	}

	fast := picker.Wait()
	if fast != nil {
		u.fast = fast.(C.Proxy)
	}

	<-ctx.Done()
}

func NewURLTest(option URLTestOption, proxies []C.Proxy) (*URLTest, error) {
	_, err := urlToMetadata(option.URL)
	if err != nil {
		return nil, err
	}
	if len(proxies) < 1 {
		return nil, errors.New("The number of proxies cannot be 0")
	}

	interval := time.Duration(option.Interval) * time.Second

	var prefer Prefer
	switch strings.ToLower(option.Prefer) {
	case "":
		prefer = Fast
	case "fast":
		prefer = Fast
	case "stable":
		prefer = Stable
	default:
		return nil, errors.New("Do not support prefer optional: " + option.Prefer)
	}

	urlTest := &URLTest{
		Base: &Base{
			name: option.Name,
			tp:   C.URLTest,
		},
		proxies:  proxies[:],
		rawURL:   option.URL,
		fast:     proxies[0],
		interval: interval,
		done:     make(chan struct{}),
		once:     0,
		prefer:   prefer,
	}
	go urlTest.loop()
	return urlTest, nil
}
