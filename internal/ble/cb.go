//go:build darwin

package ble

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tinygo-org/cbgo"
)

// cbClient 直接用 cbgo（CoreBluetooth 的 Go 绑定）实现中央端。
//
// 为什么不用 tinygo.org/x/bluetooth：它在 darwin 上把 didDiscoverServices 的
// NSError 丢掉，只回一句 "service not found"，排障时等于没有信息；此外发现流程
// 用 channel + 集中缓存，出错路径也不可观测。cbgo 把 NSError 原样交出来，且
// 扫描/连接/发现/订阅/写都是薄封装，够用。
type cbClient struct {
	h  Handler
	cm cbgo.CentralManager
	cd *cbCentralDelegate
	pd *cbPeripheralDelegate

	mu        sync.Mutex
	prph      cbgo.Peripheral
	ctrl      cbgo.Characteristic
	event     cbgo.Characteristic
	audio     cbgo.Characteristic
	connected bool

	// 等待通道（构造时创建一次；每次操作前 drain，避免读到上次的残留）。
	stateCh  chan struct{}
	scanCh   chan cbScanHit
	connCh   chan error
	svcCh    chan error
	chrCh    chan error
	notifyCh chan cbNotifyResult
	writeCh  chan error
	readyCh  chan struct{}
}

type cbScanHit struct {
	prph cbgo.Peripheral
	name string
	adv  []string
	rssi int
}

type cbNotifyResult struct {
	uuid string
	err  error
}

func newCB(h Handler) *cbClient {
	c := &cbClient{
		h:        h,
		stateCh:  make(chan struct{}, 1),
		scanCh:   make(chan cbScanHit, 16),
		connCh:   make(chan error, 4),
		svcCh:    make(chan error, 4),
		chrCh:    make(chan error, 4),
		notifyCh: make(chan cbNotifyResult, 8),
		writeCh:  make(chan error, 4),
		readyCh:  make(chan struct{}, 1),
	}
	c.cm = cbgo.NewCentralManager(nil)
	c.cd = &cbCentralDelegate{c: c}
	c.cm.SetDelegate(c.cd)
	return c
}

// Enable 等 CoreBluetooth 就绪。首次运行会弹蓝牙授权，TCC 需要时间。
func (c *cbClient) Enable() error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		switch state := c.cm.State(); state {
		case cbgo.ManagerStatePoweredOn:
			return nil
		case cbgo.ManagerStateUnauthorized:
			return errors.New("蓝牙未授权：系统设置 → 隐私与安全性 → 蓝牙 里允许本程序后重试")
		case cbgo.ManagerStateUnsupported:
			return errors.New("本机不支持 BLE")
		case cbgo.ManagerStatePoweredOff:
			c.h.log("蓝牙已关闭，请打开蓝牙")
		}
		if time.Now().After(deadline) {
			return errors.New("等待蓝牙就绪超时：若刚弹过授权框，请允许后重跑")
		}
		select {
		case <-c.stateCh:
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Scan 扫描指定广播名的设备。
func (c *cbClient) Scan(name string, timeoutMillis int) (Target, error) {
	drain(c.scanCh)
	c.cm.Scan(nil, &cbgo.CentralManagerScanOpts{AllowDuplicates: false})
	defer c.cm.StopScan()

	timeout := time.Duration(timeoutMillis) * time.Millisecond
	deadline := time.After(timeout)
	seen := 0
	for {
		select {
		case hit := <-c.scanCh:
			seen++
			if hit.name != name {
				continue
			}
			c.h.log("发现设备 %s，标识 %s，RSSI %d dBm", name, hit.prph.Identifier().String(), hit.rssi)
			return Target{
				Addr:  hit.prph.Identifier().String(),
				RSSI:  int16(hit.rssi),
				Adv:   hit.adv,
				inner: hit.prph,
			}, nil
		case <-deadline:
			if seen == 0 {
				return Target{}, fmt.Errorf("%w：%s 内没扫到任何设备（蓝牙是否被别的程序占用？）", ErrScanTimeout, timeout)
			}
			return Target{}, fmt.Errorf("%w：扫到 %d 个设备，没有广播名 %q 的那个", ErrScanTimeout, seen, name)
		}
	}
}

// Connect 连接到扫描命中的设备。
func (c *cbClient) Connect(t Target) error {
	prph, ok := t.inner.(cbgo.Peripheral)
	if !ok {
		return errors.New("ble: Target 不是 cb 实现产生的句柄")
	}
	c.mu.Lock()
	c.prph = prph
	c.mu.Unlock()

	c.pd = &cbPeripheralDelegate{c: c}
	prph.SetDelegate(c.pd)

	drain(c.connCh)
	c.cm.Connect(prph, &cbgo.CentralManagerConnectOpts{
		NotifyOnConnection:    true,
		NotifyOnDisconnection: true,
		NotifyOnNotification:  true,
	})
	select {
	case err := <-c.connCh:
		if err != nil {
			return fmt.Errorf("ble: 连接失败: %w", err)
		}
	case <-time.After(15 * time.Second):
		return errors.New("ble: 连接超时")
	}
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	c.h.log("已连接 %s", t.Addr)
	// 给 GATT 一点就绪时间：刚连上立刻做 ATT 发现偶发拿不到服务表。
	time.Sleep(400 * time.Millisecond)
	return nil
}

// Discover 全量发现服务与特征，并把所有 UUID 打出来（失败时这就是唯一线索）。
func (c *cbClient) Discover() (GATT, error) {
	var gatt GATT
	if !c.Connected() {
		return gatt, ErrNotConnected
	}
	prph := c.peripheral()

	var svcs []cbgo.Service
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		drain(c.svcCh)
		prph.DiscoverServices(nil)
		select {
		case err := <-c.svcCh:
			if err != nil {
				lastErr = err
				c.h.log("didDiscoverServices 报错（第 %d 次）：%v", attempt, err)
			}
		case <-time.After(10 * time.Second):
			lastErr = errors.New("等不到 didDiscoverServices 回调")
			c.h.log("%v（第 %d 次）", lastErr, attempt)
		}
		if svcs = prph.Services(); len(svcs) > 0 {
			break
		}
		c.h.log("第 %d 次发现：服务表为空", attempt)
		time.Sleep(500 * time.Millisecond)
	}
	if len(svcs) == 0 {
		if lastErr != nil {
			return gatt, fmt.Errorf("%w（最后一次错误：%v）", ErrNoService, lastErr)
		}
		return gatt, ErrNoService
	}

	type svcInfo struct {
		svc   cbgo.Service
		uuid  string
		chars []cbgo.Characteristic
	}
	infos := make([]svcInfo, 0, len(svcs))
	for _, svc := range svcs {
		uuid := strings.ToUpper(svc.UUID().String())
		gatt.Services = append(gatt.Services, uuid)
		drain(c.chrCh)
		prph.DiscoverCharacteristics(nil, svc)
		select {
		case err := <-c.chrCh:
			if err != nil {
				c.h.log("服务 %s 的 didDiscoverCharacteristics 报错：%v", uuid, err)
			}
		case <-time.After(10 * time.Second):
			c.h.log("服务 %s 等不到 didDiscoverCharacteristics 回调", uuid)
		}
		infos = append(infos, svcInfo{svc: svc, uuid: uuid, chars: svc.Characteristics()})
	}

	// 先按服务 UUID 匹配；CoreBluetooth 有时把它收回成 16 位，所以退一步用
	// "哪个服务里有 CTRL" 认领。
	idx := -1
	for i := range infos {
		if uuid16(infos[i].uuid) == uuid16(ServiceUUID) {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i := range infos {
			uuids := make([]string, 0, len(infos[i].chars))
			for _, chr := range infos[i].chars {
				uuids = append(uuids, chr.UUID().String())
			}
			if hasCTRL(uuids) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return gatt, fmt.Errorf("%w：实际发现 %v", ErrNoService, gatt.Services)
	}

	gatt.Service = infos[idx].uuid
	var hasEvent, hasAudio bool
	for _, chr := range infos[idx].chars {
		uuid := strings.ToUpper(chr.UUID().String())
		gatt.Chars = append(gatt.Chars, fmt.Sprintf("%s（%s）", uuid, uuid16(uuid)))
		switch uuid16(uuid) {
		case uuid16(CtrlUUID):
			c.ctrl = chr
		case uuid16(EventUUID):
			c.event, hasEvent = chr, true
		case uuid16(AudioUUID):
			c.audio, hasAudio = chr, true
		}
	}
	if !hasEvent || !hasAudio {
		return gatt, fmt.Errorf("%w（EVENT=%v AUDIO=%v，服务 %s 的特征 %v）",
			ErrNoChar, hasEvent, hasAudio, gatt.Service, gatt.Chars)
	}
	gatt.WriteLimit = prph.MaximumWriteValueLength(false)
	return gatt, nil
}

// Subscribe 订阅 EVENT 与 AUDIO。回调在 CoreBluetooth 队列线程上执行。
func (c *cbClient) Subscribe() error {
	prph := c.peripheral()
	for _, item := range []struct {
		chr  cbgo.Characteristic
		name string
	}{
		{c.event, "EVENT"},
		{c.audio, "AUDIO"},
	} {
		drain(c.notifyCh)
		prph.SetNotify(true, item.chr)
		select {
		case res := <-c.notifyCh:
			if res.err != nil {
				return fmt.Errorf("ble: 订阅 %s 失败: %w", item.name, res.err)
			}
		case <-time.After(10 * time.Second):
			return fmt.Errorf("ble: 订阅 %s 超时", item.name)
		}
	}
	c.h.log("已订阅 EVENT / AUDIO")
	return nil
}

// WriteCTRL 写一行 CTRL（无响应写，与固件 WRITE_NO_RSP 一致）。
func (c *cbClient) WriteCTRL(line []byte) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	if len(line) > 2048 {
		return fmt.Errorf("ble: CTRL 载荷 %d B 超过 2048 B 上限", len(line))
	}
	prph := c.peripheral()
	if !prph.CanSendWriteWithoutResponse() {
		select {
		case <-c.readyCh:
		case <-time.After(500 * time.Millisecond):
			return errors.New("ble: 控制器写缓冲忙，稍后重试")
		}
	}
	prph.WriteCharacteristic(line, c.ctrl, false)
	return nil
}

// WriteCTRLConfirmed 带响应地写一行 CTRL，用来确认加密链路是否就绪。
//
// CTRL 带 WRITE_ENC：未完成配对时写入被拒（insufficientAuthentication /
// insufficientEncryption）；无响应写拿不到这个错误，所以配对探测必须走这条。
func (c *cbClient) WriteCTRLConfirmed(line []byte) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	drain(c.writeCh)
	c.peripheral().WriteCharacteristic(line, c.ctrl, true)
	select {
	case err := <-c.writeCh:
		if err != nil {
			return fmt.Errorf("ble: CTRL 确认写失败（多为配对/加密未完成，去 系统设置 → 蓝牙 删除该设备后重连）: %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("ble: 等不到写入确认回调")
	}
}

// Connected 报告连接状态。
func (c *cbClient) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected && c.prph != (cbgo.Peripheral{})
}

// Close 断开连接。
func (c *cbClient) Close() error {
	c.mu.Lock()
	prph := c.prph
	was := c.connected
	c.connected = false
	c.mu.Unlock()
	if !was {
		return nil
	}
	c.cm.CancelConnect(prph)
	return nil
}

func (c *cbClient) peripheral() cbgo.Peripheral {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prph
}

// ---- delegate ----

type cbCentralDelegate struct {
	cbgo.CentralManagerDelegateBase
	c *cbClient
}

func (d *cbCentralDelegate) CentralManagerDidUpdateState(cmgr cbgo.CentralManager) {
	select {
	case d.c.stateCh <- struct{}{}:
	default:
	}
}

func (d *cbCentralDelegate) DidDiscoverPeripheral(cmgr cbgo.CentralManager, prph cbgo.Peripheral, adv cbgo.AdvFields, rssi int) {
	advSvcs := make([]string, 0, len(adv.ServiceUUIDs))
	for _, u := range adv.ServiceUUIDs {
		advSvcs = append(advSvcs, strings.ToUpper(u.String()))
	}
	select {
	case d.c.scanCh <- cbScanHit{prph: prph, name: adv.LocalName, adv: advSvcs, rssi: rssi}:
	default:
	}
}

func (d *cbCentralDelegate) DidConnectPeripheral(cmgr cbgo.CentralManager, prph cbgo.Peripheral) {
	select {
	case d.c.connCh <- nil:
	default:
	}
}

func (d *cbCentralDelegate) DidFailToConnectPeripheral(cmgr cbgo.CentralManager, prph cbgo.Peripheral, err error) {
	select {
	case d.c.connCh <- err:
	default:
	}
}

func (d *cbCentralDelegate) DidDisconnectPeripheral(cmgr cbgo.CentralManager, prph cbgo.Peripheral, err error) {
	d.c.mu.Lock()
	d.c.connected = false
	d.c.mu.Unlock()
	if err != nil {
		d.c.h.log("连接已断开: %v", err)
	} else {
		d.c.h.log("连接已断开")
	}
	if d.c.h.OnDisconnect != nil {
		d.c.h.OnDisconnect(err)
	}
}

type cbPeripheralDelegate struct {
	cbgo.PeripheralDelegateBase
	c *cbClient
}

func (d *cbPeripheralDelegate) DidDiscoverServices(prph cbgo.Peripheral, err error) {
	select {
	case d.c.svcCh <- err:
	default:
	}
}

func (d *cbPeripheralDelegate) DidDiscoverCharacteristics(prph cbgo.Peripheral, svc cbgo.Service, err error) {
	select {
	case d.c.chrCh <- err:
	default:
	}
}

func (d *cbPeripheralDelegate) DidUpdateNotificationState(prph cbgo.Peripheral, chr cbgo.Characteristic, err error) {
	select {
	case d.c.notifyCh <- cbNotifyResult{uuid: chr.UUID().String(), err: err}:
	default:
	}
}

func (d *cbPeripheralDelegate) DidWriteValueForCharacteristic(prph cbgo.Peripheral, chr cbgo.Characteristic, err error) {
	select {
	case d.c.writeCh <- err:
	default:
	}
}

func (d *cbPeripheralDelegate) DidUpdateValueForCharacteristic(prph cbgo.Peripheral, chr cbgo.Characteristic, err error) {
	if err != nil {
		return
	}
	// 回调线程只做分发：chr.Value() 已经是 Go 内存拷贝。
	switch uuid16(chr.UUID().String()) {
	case uuid16(EventUUID):
		if d.c.h.OnEvent != nil {
			d.c.h.OnEvent(chr.Value())
		}
	case uuid16(AudioUUID):
		if d.c.h.OnAudio != nil {
			d.c.h.OnAudio(chr.Value())
		}
	}
}

func (d *cbPeripheralDelegate) IsReadyToSendWriteWithoutResponse(prph cbgo.Peripheral) {
	select {
	case d.c.readyCh <- struct{}{}:
	default:
	}
}

// drain 清空通道里的残留信号。
func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
