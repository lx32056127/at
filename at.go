package at

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/lx32056127/at/pdu"
	"github.com/lx32056127/at/sms"
	"github.com/tarm/goserial"
)

// BaudRate defines the default speed of serial connection.
const BaudRate = 115200

// Timeout to close the connection in case of modem is being not responsive at all.
const Timeout = time.Minute

// Sep <CR><LF> sequence.
const Sep = "\r\n"

// Sub Ctrl+Z code.
const Sub = string(0x1A)

// Common errors.
var (
	ErrTimeout         = errors.New("at: timeout")
	ErrUnknownEncoding = errors.New("at: unsupported encoding")
	ErrClosed          = errors.New("at: device ports are closed")
	ErrNotInitialized  = errors.New("at: not initialized")
	ErrWriteFailed     = errors.New("at: write failed")
	ErrParseReport     = errors.New("at: error while parsing report")
	ErrUnknownReport   = errors.New("at: got unknown report")
)

// Encoding is an encoding option to use.
type Encoding byte

// Encodings represents all the supported encodings.
var Encodings = struct {
	Gsm7Bit Encoding
	UCS2    Encoding
}{
	15, 72,
}

// Device represents a physical modem that supports Hayes AT-commands.
type Device struct {
	// Name is the label to distinguish different devices.
	Name string
	// CommandPort is the path or name of command serial port.
	CommandPort string
	// CommandPort is the path or name of notification serial port.
	NotifyPort string
	// State holds the device state.
	State *DeviceState
	// Commands is a profile that provides implementation of Init and the other commands.
	Commands DeviceProfile

	cmdPort    io.ReadWriteCloser
	notifyPort io.ReadWriteCloser

	messages chan *sms.Message
	ussd     chan Ussd
	updated  chan struct{}
	closed   chan struct{}

	active bool
}

// IncomingSms fires when an SMS was received.
func (d *Device) IncomingSms() <-chan *sms.Message {
	return d.messages
}

// UssdReply fires when an Ussd reply was received.
func (d *Device) UssdReply() <-chan Ussd {
	return d.ussd
}

// StateUpdate fires when DeviceState was updated by a received event.
func (d *Device) StateUpdate() <-chan struct{} {
	return d.updated
}

// Closed fires when the connection was closed.
func (d *Device) Closed() <-chan struct{} {
	return d.closed
}

// sendInteractive is a special case of Send, but this one is used whether
// a prompt should be received first (i.e. when sending SMS, the PDU should be
// entered after the device replied with '>') and then the second part of payload
// should be sent (the second payload will be sent using Send).
func (d *Device) sendInteractive(part1, part2 string, prompt byte) (err error) {
	t := time.NewTimer(Timeout)
	defer t.Stop()

	exitInteractive := func() { d.cmdPort.Write([]byte{pdu.Esc}) }

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-stop:
			return
		case <-t.C:
			exitInteractive()
			d.cmdPort.Write([]byte(KillCmd + Sep))
		}
	}()

	_, err = d.cmdPort.Write([]byte(part1 + Sep))
	if err != nil {
		return
	}

	buf := bufio.NewReader(d.cmdPort)
	line, err := buf.ReadString(prompt)
	if err != nil || !strings.HasSuffix(line, string(prompt)) {
		exitInteractive()
		return
	}

	_, err = d.Send(part2 + Sub)
	if err != nil {
		exitInteractive()
		return
	}
	return
}

// sanityCheck checks whether ports are opened and (if requested) that the initialization
// was done.
func (d *Device) sanityCheck(initialized bool) error {
	if d.cmdPort == nil {
		return ErrClosed
	}
	if d.notifyPort == nil {
		return ErrClosed
	}
	if initialized {
		if d.Commands == nil {
			return ErrNotInitialized
		}
	}
	return nil
}

// Send writes a command to the device's command port and parses the output.
// Result will not contain any FinalReply since they're used to detect error status.
// Multiple lines will be joined with '\n'.
func (d *Device) Send(req string) (reply string, err error) {
	if err = d.sanityCheck(true); err != nil {
		return
	}
	t := time.NewTimer(Timeout)
	defer t.Stop()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-stop:
			return
		case <-t.C:
			d.cmdPort.Write([]byte(KillCmd + Sep))
		}
	}()

	_, err = d.cmdPort.Write([]byte(req + Sep))
	if err != nil {
		return
	}

	var line string
	buf := bufio.NewReader(d.cmdPort)
	if line, err = buf.ReadString('\r'); err != nil {
		return "", ErrWriteFailed
	}
	text := strings.TrimSpace(line)
	if !strings.HasPrefix(req, text) {
		return "", ErrWriteFailed
	}
	t.Reset(Timeout)

	var done bool
	for !done {
		if line, err = buf.ReadString('\r'); err != nil {
			err = io.EOF
			break
		}
		text := strings.TrimSpace(line)
		if len(text) < 1 {
			continue
		}
		switch opt := FinalResults.Resolve(text); opt {
		case FinalResults.Ok, FinalResults.Noop:
			done = true
		case FinalResults.Timeout:
			err = ErrTimeout
			done = true
		case FinalResults.CmeError, FinalResults.CmsError:
			err = errors.New(text)
			done = true
		case FinalResults.Error, FinalResults.NotSupported,
			FinalResults.TooManyParameters, FinalResults.NoCarrier:
			err = errors.New(opt.Description)
			done = true
		default:
			if len(reply) > 0 {
				reply += "\n"
			}
			reply += text
			t.Reset(Timeout)
		}
	}
	return
}

// Watch starts a monitoring process that will wait for events
// from the device's notification port.
func (d *Device) Watch() error {
	if d.notifyPort == nil {
		return errors.New("at: notification port not initialized")
	}
	go func() {
		<-d.closed
		d.notifyPort.Write([]byte(KillCmd + Sep))
	}()

	buf := bufio.NewReader(d.notifyPort)
	for {
		select {
		case <-d.closed:
			return nil
		default:
			line, err := buf.ReadString(byte('\r'))
			if err != nil {
				d.Close()
				return nil
			}
			text := strings.TrimSpace(line)
			if len(text) < 1 {
				continue
			}
			d.handleReport(text) // ignore errors
		}
	}
}

// handleReport detects and parses a report from the notification port represented
// as a string. The parsed values may change the inner state or be sent over out channels.
func (d *Device) handleReport(str string) (err error) {
	report := Reports.Resolve(str)
	str = strings.TrimSpace(strings.TrimPrefix(str, report.ID))
	switch report {
	case Reports.Message:
		var report messageReport
		if err = report.Parse(str); err != nil {
			return
		}
		var octets []byte
		octets, err = d.Commands.CMGR(report.Index)
		if err != nil {
			return
		}
		if err = d.Commands.CMGD(report.Index, DeleteOptions.Index); err != nil {
			return
		}
		var msg sms.Message
		if _, err = msg.ReadFrom(octets); err != nil {
			return
		}
		d.messages <- &msg
	case Reports.Ussd:
		var ussd ussdReport
		if err = ussd.Parse(str); err != nil {
			return
		}
		var text string
		if ussd.Enc == Encodings.UCS2 {
			text, err = pdu.DecodeUcs2(ussd.Octets, false)
			if err != nil {
				return
			}
		} else if ussd.Enc == Encodings.Gsm7Bit {
			text, err = pdu.Decode7Bit(ussd.Octets)
			if err != nil {
				return
			}
		} else {
			return ErrUnknownEncoding
		}
		d.ussd <- Ussd(text)
	case Reports.SignalStrength:
		var rssi signalStrengthReport
		if err = rssi.Parse(str); err != nil {
			return
		}
		if d.State.SignalStrength != int(rssi) {
			d.State.SignalStrength = int(rssi)
			d.updated <- struct{}{}
		}
	case Reports.Mode:
		var report modeReport
		if err = report.Parse(str); err != nil {
			return
		}
		var updated bool
		if d.State.SystemMode != report.Mode {
			d.State.SystemMode = report.Mode
			updated = true
		}
		if d.State.SystemSubmode != report.Submode {
			d.State.SystemSubmode = report.Submode
			updated = true
		}
		if updated {
			d.updated <- struct{}{}
		}
	case Reports.ServiceState:
		var report serviceStateReport
		if err = report.Parse(str); err != nil {
			return
		}
		if d.State.ServiceState != Opt(report) {
			d.State.ServiceState = Opt(report)
			d.updated <- struct{}{}
		}
	case Reports.SimState:
		var report simStateReport
		if err = report.Parse(str); err != nil {
			return
		}
		if d.State.SimState != Opt(report) {
			d.State.SimState = Opt(report)
			d.updated <- struct{}{}
		}
	case Reports.BootHandshake:
		var token bootHandshakeReport
		if err = token.Parse(str); err != nil {
			return
		}
		if err = d.Commands.BOOT(uint64(token)); err != nil {
			return
		}
	case Reports.Stin:
		// ignore. what is this btw?
	default:
		switch FinalResults.Resolve(str) {
		case FinalResults.Noop, FinalResults.NotSupported, FinalResults.Timeout:
			// ignore
		default:
			return errors.New("at: unknown report: " + str)
		}
	}
	return
}

// Open is used to open serial ports of the device. This should be used first.
// The method returns error if open was not succeed, i.e. if device is absent.
func (d *Device) Open() (err error) {
	if d.cmdPort, err = serial.OpenPort(&serial.Config{
		Name: d.CommandPort,
		Baud: BaudRate,
	}); err != nil {
		return
	}
	if len(d.NotifyPort) > 0 && d.NotifyPort != d.CommandPort {
		if d.notifyPort, err = serial.OpenPort(&serial.Config{
			Name: d.NotifyPort,
			Baud: BaudRate,
		}); err != nil {
			return
		}
	}
	return
}

// Init checks whether device is opened, initializes event channels
// and runs init procedure defined within the supplied DeviceProfile.
func (d *Device) Init(profile DeviceProfile) error {
	if err := d.sanityCheck(false); err != nil {
		return err
	}
	d.active = true
	d.closed = make(chan struct{})
	d.messages = make(chan *sms.Message, 100)
	d.ussd = make(chan Ussd, 100)
	d.updated = make(chan struct{}, 100)
	d.Commands = profile
	return profile.Init(d)
}

// Close closes all the event channels and also closes
// both command and notification modem's ports. This function may block
// until the device will reply or the reply timeout timer will fire.
//
// Close is a no-op if already closed.
func (d *Device) Close() (err error) {
	if d.active {
		d.active = false
		close(d.closed)
	}
	if d.cmdPort != nil {
		err = d.cmdPort.Close()
	}
	if d.notifyPort != nil {
		if err2 := d.notifyPort.Close(); err2 != nil {
			err = err2
		}
	}
	return
}

// SendUSSD sends an USSD request, the encoding and other parameters are default.
func (d *Device) SendUSSD(req string) (err error) {
	err = d.Commands.CUSD(UssdResultReporting.Enable, pdu.Encode7Bit(req), Encodings.Gsm7Bit)
	return
}

// SendSMS sends an SMS message with given text to the given address,
// the encoding and other parameters are default.
func (d *Device) SendSMS(text string, address sms.PhoneNumber) (err error) {
	msg := sms.Message{
		Text:     text,
		Type:     sms.MessageTypes.Submit,
		Encoding: sms.Encodings.Gsm7Bit,
		Address:  address,
		VPFormat: sms.ValidityPeriodFormats.Relative,
		VP:       sms.ValidityPeriod(24 * time.Hour * 4),
	}
	for _, w := range text {
		// detected a double-width char
		if w > 1 {
			msg.Encoding = sms.Encodings.UCS2
			break
		}
	}
	n, octets, err := msg.PDU()
	if err != nil {
		return err
	}
	err = d.Commands.CMGS(n, octets)
	return
}
