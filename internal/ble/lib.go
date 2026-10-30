//go:build blelib

package ble

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// libClient 是 tinygo.org/x/bluetooth 实现，只作对照用。
//
// 它在 darwin 上会把 didDiscoverServices 的 NSError 吞掉（只回 ErrServiceNotFound），
// 排障时看不见原因；默认不用它，保留是为了能一条命令切换对照。
type libClient struct {
	h       Handler
	adapter *bluetooth.Adapter

	mu        sync.Mutex
	dev       bluetooth.Device
	ctrl      bluetooth.DeviceCharacteristic
	event     bluetooth.DeviceCharacteristic
	audio     bluetooth.DeviceCharacteristic
	connected bool
	enabled   bool
	adv       []string
	rssi      int16
}

func init() {
	newLibImpl = func(h Handler) Client { return &libClient{h: h, adapter: bluetooth.DefaultAdapter} }
}

func newLib(h Handler) *libClient {
	return &libClient{h: h, adapter: bluetooth.DefaultAdapter}
}

func (c *libClient) Enable() error {
	c.mu.Lock()
	already := c.enabled
	c.mu.Unlock()
	if already {
		return nil
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := c.adapter.Enable()
		if err == nil {
			c.mu.Lock()
			c.enabled = true
			c.mu.Unlock()
			return nil
		}
		lastErr = err
		c.h.log("蓝牙适配器启用失败（第 %d 次）：%v", attempt, err)
		time.Sleep(time.Second)
	}
	return fmt.Errorf("ble(lib): 启用蓝牙失败（%w）", lastErr)
}

func (c *libClient) Scan(name string, timeoutMillis int) (Target, error) {
	type hit struct {
		addr bluetooth.Address
		adv  []string
		rssi int16
	}
	found := make(chan hit, 1)
	scanDone := make(chan error, 1)

	go func() {
		scanDone <- c.adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			if r.LocalName() != name {
				return
			}
			adv := make([]string, 0, 4)
			for _, u := range r.ServiceUUIDs() {
				adv = append(adv, strings.ToUpper(u.String()))
			}
			select {
			case found <- hit{addr: r.Address, adv: adv, rssi: r.RSSI}:
			default:
			}
		})
	}()

	timer := time.NewTimer(time.Duration(timeoutMillis) * time.Millisecond)
	defer timer.Stop()

	var t Target
	var addr bluetooth.Address
	var err error
	select {
	case h := <-found:
		addr = h.addr
		t.Adv, t.RSSI = h.adv, h.rssi
	case <-timer.C:
		err = ErrScanTimeout
	case scanErr := <-scanDone:
		if scanErr != nil {
			err = scanErr
		} else {
			err = ErrNotFound
		}
	}
	// 只在这里调 StopScan：库里两个 goroutine 同时 StopScan 会撞成 send on closed channel。
	_ = c.adapter.StopScan()
	select {
	case <-scanDone:
	case <-time.After(3 * time.Second):
	}
	if err != nil {
		return Target{}, err
	}
	t.Addr = addr.String()
	t.inner = addr
	return t, nil
}

func (c *libClient) Connect(t Target) error {
	addr, ok := t.inner.(bluetooth.Address)
	if !ok {
		return errors.New("ble(lib): Target 不是 lib 实现产生的句柄")
	}
	dev, err := c.adapter.Connect(addr, bluetooth.ConnectionParams{})
	if err != nil {
		return fmt.Errorf("ble(lib): 连接失败: %w", err)
	}
	c.mu.Lock()
	c.dev = dev
	c.connected = true
	c.mu.Unlock()
	time.Sleep(400 * time.Millisecond)
	return nil
}

func (c *libClient) Discover() (GATT, error) {
	var gatt GATT
	if !c.Connected() {
		return gatt, ErrNotConnected
	}
	svcs, err := c.discoverServices()
	if err != nil {
		return gatt, err
	}
	for i := range svcs {
		gatt.Services = append(gatt.Services, strings.ToUpper(svcs[i].UUID().String()))
	}
	idx := -1
	for i := range svcs {
		if uuid16(svcs[i].UUID().String()) == uuid16(ServiceUUID) {
			idx = i
			break
		}
	}
	var chars []bluetooth.DeviceCharacteristic
	if idx >= 0 {
		if chars, err = svcs[idx].DiscoverCharacteristics(nil); err != nil {
			return gatt, fmt.Errorf("ble(lib): 发现特征失败: %w", err)
		}
	}
	if !libHasCTRL(chars) {
		for i := range svcs {
			if i == idx {
				continue
			}
			cs, cerr := svcs[i].DiscoverCharacteristics(nil)
			if cerr != nil {
				continue
			}
			if libHasCTRL(cs) {
				idx, chars = i, cs
				break
			}
		}
	}
	if idx < 0 {
		return gatt, fmt.Errorf("%w：实际发现 %v", ErrNoService, gatt.Services)
	}
	gatt.Service = strings.ToUpper(svcs[idx].UUID().String())

	var hasEvent, hasAudio bool
	for i := range chars {
		uuid := strings.ToUpper(chars[i].UUID().String())
		gatt.Chars = append(gatt.Chars, fmt.Sprintf("%s（%s）", uuid, uuid16(uuid)))
		switch uuid16(uuid) {
		case uuid16(CtrlUUID):
			c.ctrl = chars[i]
		case uuid16(EventUUID):
			c.event, hasEvent = chars[i], true
		case uuid16(AudioUUID):
			c.audio, hasAudio = chars[i], true
		}
	}
	if !hasEvent || !hasAudio {
		return gatt, fmt.Errorf("%w（EVENT=%v AUDIO=%v）", ErrNoChar, hasEvent, hasAudio)
	}
	if limit, err := c.ctrl.GetMTU(); err == nil {
		gatt.WriteLimit = int(limit)
	}
	return gatt, nil
}

func (c *libClient) discoverServices() ([]bluetooth.DeviceService, error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		svcs, err := c.dev.DiscoverServices(nil)
		if err != nil {
			lastErr = err
			c.h.log("ble(lib) 发现服务失败（第 %d 次）：%v", attempt, err)
		} else if len(svcs) > 0 {
			return svcs, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, s := range []string{ServiceUUID, "A2B0"} {
		u, err := bluetooth.ParseUUID(s)
		if err != nil {
			continue
		}
		svcs, err := c.dev.DiscoverServices([]bluetooth.UUID{u})
		if err != nil {
			c.h.log("ble(lib) 按 %s 过滤发现：%v", s, err)
			continue
		}
		if len(svcs) > 0 {
			return svcs, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("ble(lib): 发现服务失败: %w", lastErr)
	}
	return nil, ErrNoService
}

func libHasCTRL(chars []bluetooth.DeviceCharacteristic) bool {
	uuids := make([]string, 0, len(chars))
	for i := range chars {
		uuids = append(uuids, chars[i].UUID().String())
	}
	return hasCTRL(uuids)
}

func (c *libClient) Subscribe() error {
	if err := c.event.EnableNotifications(func(buf []byte) {
		if c.h.OnEvent != nil {
			c.h.OnEvent(buf)
		}
	}); err != nil {
		return fmt.Errorf("ble(lib): 订阅 EVENT 失败: %w", err)
	}
	if err := c.audio.EnableNotifications(func(buf []byte) {
		if c.h.OnAudio != nil {
			c.h.OnAudio(buf)
		}
	}); err != nil {
		return fmt.Errorf("ble(lib): 订阅 AUDIO 失败: %w", err)
	}
	return nil
}

func (c *libClient) WriteCTRL(line []byte) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	if len(line) > 2048 {
		return fmt.Errorf("ble(lib): CTRL 载荷 %d B 超过上限", len(line))
	}
	if _, err := c.ctrl.WriteWithoutResponse(line); err != nil {
		return fmt.Errorf("ble(lib): CTRL 写入失败: %w", err)
	}
	return nil
}

func (c *libClient) WriteCTRLConfirmed(line []byte) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	if _, err := c.ctrl.Write(line); err != nil {
		return fmt.Errorf("ble(lib): CTRL 确认写失败: %w", err)
	}
	return nil
}

func (c *libClient) Connected() bool {
	c.mu.Lock()
	connected := c.connected
	dev := c.dev
	c.mu.Unlock()
	if !connected {
		return false
	}
	ok, err := dev.Connected()
	return err == nil && ok
}

func (c *libClient) Close() error {
	c.mu.Lock()
	was := c.connected
	c.connected = false
	dev := c.dev
	c.mu.Unlock()
	if !was {
		return nil
	}
	return dev.Disconnect()
}
