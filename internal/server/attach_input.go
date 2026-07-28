package server

import (
	"errors"
	"os"
	"time"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

// menuEscapeGrace is how long an escape waits for the rest of a key before it
// is taken to be the Esc key itself. A terminal may split an arrow key across
// reads, so a read boundary cannot be used to tell them apart.
const menuEscapeGrace = 60 * time.Millisecond

// sessionSwitch names where the switcher should move. A step of -1 or +1 walks
// the running sessions; a step of 0 selects index directly.
type sessionSwitch struct {
	step  int
	index int
}

type attachInputHandlers struct {
	prefix                byte
	notificationsMuted    bool
	supportsNotifications bool
	onNotificationsMuted  func(bool)
	onNewSession          func()
	onDetach              func()
	onSwitch              func(sessionSwitch)
	onPrefixArmed         func(bool)
	menu                  menuController
}

// menuController is the session switcher, as the input reader needs it. While
// the switcher is open it owns the keyboard: nothing reaches the session, so an
// arrow key moves the selection rather than the agent's cursor.
type menuController interface {
	isOpen() bool
	toggle()
	move(delta int)
	selectIndex(index int)
	choose()
	close()
}

// prefixMode tracks how far through a prefixed key the reader has read. The
// arrow keys arrive as three bytes that a terminal is free to split across
// reads, so the position has to survive between them.
type prefixMode uint8

const (
	prefixIdle prefixMode = iota
	prefixArmed
	prefixEscape
	prefixSequence
)

func readAttachInput(input *os.File, writer *attachFrameWriter, handlers attachInputHandlers) {
	reader := &attachInputReader{writer: writer, handlers: handlers}
	buffer := make([]byte, 32*1024)
	for {
		deadline := time.Time{}
		if reader.menuEscapePending() {
			deadline = time.Now().Add(menuEscapeGrace)
		}
		// A terminal that cannot carry a deadline simply blocks, and the pending
		// escape resolves on the next key instead of on a timer.
		_ = input.SetReadDeadline(deadline)

		count, err := input.Read(buffer)
		if count > 0 && !reader.consume(buffer[:count]) {
			return
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				reader.resolveMenuEscape()
				continue
			}
			return
		}
	}
}

type attachInputReader struct {
	writer        *attachFrameWriter
	handlers      attachInputHandlers
	mode          prefixMode
	introducer    byte
	menuMode      prefixMode
	out           []byte
	armedReported bool
}

// menuEscapePending reports that the switcher has read an escape and does not
// yet know whether it begins a key or is the Esc key.
func (r *attachInputReader) menuEscapePending() bool {
	return r.menuMode == prefixEscape
}

// resolveMenuEscape settles a pending escape as the Esc key.
func (r *attachInputReader) resolveMenuEscape() {
	if r.menuMode != prefixEscape {
		return
	}
	r.menuMode = prefixIdle
	if r.handlers.menu != nil {
		r.handlers.menu.close()
	}
}

// reportPrefixState tells the footer whether Falkn is holding the prefix and
// waiting for the key that completes it. It runs once per read rather than per
// byte, so a prefixed key delivered whole never flickers the indicator on and
// straight back off.
func (r *attachInputReader) reportPrefixState() {
	armed := r.mode != prefixIdle
	if armed == r.armedReported || r.handlers.onPrefixArmed == nil {
		r.armedReported = armed
		return
	}
	r.armedReported = armed
	r.handlers.onPrefixArmed(armed)
}

// consume forwards a read to the session, intercepting prefixed keys. It
// reports whether the caller should keep reading.
func (r *attachInputReader) consume(data []byte) bool {
	r.out = r.out[:0]
	for index := 0; index < len(data); index++ {
		value := data[index]
		if r.handlers.menu == nil || !r.handlers.menu.isOpen() {
			r.menuMode = prefixIdle
		} else if r.mode == prefixIdle && r.consumeMenuByte(value) {
			continue
		}
		if r.mode == prefixIdle && value == 0x1b && index+2 < len(data) &&
			data[index+1] == '[' && (data[index+2] == 'I' || data[index+2] == 'O') {
			if !r.flush() {
				return false
			}
			_ = r.writer.focus(data[index+2] == 'I')
			index += 2
			continue
		}
		if !r.consumeByte(value) {
			r.reportPrefixState()
			return false
		}
	}
	r.reportPrefixState()
	return r.flush()
}

func (r *attachInputReader) consumeByte(value byte) (keepReading bool) {
	switch r.mode {
	case prefixArmed:
		return r.consumeArmed(value)
	case prefixEscape:
		r.mode = prefixIdle
		if value == '[' || value == 'O' {
			r.mode = prefixSequence
			r.introducer = value
			return true
		}
		// Not a key the prefix claims; hand back what was swallowed.
		r.out = append(r.out, r.handlers.prefix, 0x1b, value)
		return true
	case prefixSequence:
		r.mode = prefixIdle
		return r.consumeSequence(value)
	}
	if value == r.handlers.prefix {
		r.mode = prefixArmed
		return true
	}
	r.out = append(r.out, value)
	return true
}

func (r *attachInputReader) consumeArmed(value byte) (keepReading bool) {
	r.mode = prefixIdle
	switch {
	case value == 'q' || value == 'Q':
		_ = r.flush()
		_ = r.writer.frame(attachDetachFrame, nil)
		if r.handlers.onDetach != nil {
			r.handlers.onDetach()
		}
		_ = r.writer.connection.Close()
		return false
	case (value == 'm' || value == 'M') && r.handlers.supportsNotifications:
		if !r.flush() {
			return false
		}
		r.handlers.notificationsMuted = !r.handlers.notificationsMuted
		if r.writer.notificationsMuted(r.handlers.notificationsMuted) != nil {
			return false
		}
		if r.handlers.onNotificationsMuted != nil {
			r.handlers.onNotificationsMuted(r.handlers.notificationsMuted)
		}
		return true
	case (value == 'n' || value == 'N') && r.handlers.onNewSession != nil:
		if !r.flush() {
			return false
		}
		r.handlers.onNewSession()
		return true
	case value >= '1' && value <= '9' && r.handlers.onSwitch != nil:
		if !r.flush() {
			return false
		}
		r.handlers.onSwitch(sessionSwitch{index: int(value - '0')})
		return true
	case value == 0x1b:
		r.mode = prefixEscape
		return true
	case value == r.handlers.prefix:
		// Pressing the prefix twice sends one literal prefix byte.
		r.out = append(r.out, r.handlers.prefix)
		return true
	}
	r.out = append(r.out, r.handlers.prefix, value)
	return true
}

func (r *attachInputReader) consumeSequence(final byte) (keepReading bool) {
	switch final {
	case 'C', 'D':
		if r.handlers.onSwitch == nil {
			break
		}
		if !r.flush() {
			return false
		}
		step := 1
		if final == 'D' {
			step = -1
		}
		r.handlers.onSwitch(sessionSwitch{step: step})
		return true
	case 'A', 'B':
		if r.handlers.menu == nil {
			break
		}
		if !r.flush() {
			return false
		}
		r.handlers.menu.toggle()
		return true
	}
	r.out = append(r.out, r.handlers.prefix, 0x1b, r.introducer, final)
	return true
}

func (r *attachInputReader) flush() bool {
	if len(r.out) == 0 {
		return true
	}
	err := r.writer.frame(attachInputFrame, r.out)
	r.out = r.out[:0]
	return err == nil
}

// selectSwitchTarget picks the session a switch request lands on. Sessions are
// ordered oldest first so the sequence a user cycles through does not reshuffle
// as sessions come and go.
func selectSwitchTarget(running []protocol.SessionRecord, currentID string, request sessionSwitch) string {
	if len(running) == 0 {
		return ""
	}
	if request.step == 0 {
		if request.index < 1 || request.index > len(running) {
			return ""
		}
		return running[request.index-1].ID
	}

	current := -1
	for index, record := range running {
		if record.ID == currentID {
			current = index
			break
		}
	}
	if current < 0 {
		// The attached session is gone or not switchable; start from the end so
		// stepping forward lands on the first session.
		current = len(running) - 1
		if request.step < 0 {
			current = 0
		}
	}
	next := (current + request.step) % len(running)
	if next < 0 {
		next += len(running)
	}
	return running[next].ID
}

// consumeMenuByte handles one key for the open switcher and reports whether the
// switcher took it. An escape is held until the next byte decides it: a
// terminal may deliver an arrow key one byte at a time, and treating a lone
// escape as Esc closed the switcher and spilled the rest into the session.
func (r *attachInputReader) consumeMenuByte(value byte) bool {
	menu := r.handlers.menu
	switch r.menuMode {
	case prefixEscape:
		if value == '[' || value == 'O' {
			r.menuMode = prefixSequence
			return true
		}
		// The escape stood alone. It closes the switcher, and this byte belongs
		// to the session that is now in front again.
		r.menuMode = prefixIdle
		menu.close()
		return false
	case prefixSequence:
		r.menuMode = prefixIdle
		switch value {
		case 'A':
			menu.move(-1)
		case 'B':
			menu.move(1)
		}
		return true
	}

	switch {
	case value == 0x1b:
		r.menuMode = prefixEscape
	case value == '\r' || value == '\n':
		menu.choose()
	case value == 'k':
		menu.move(-1)
	case value == 'j':
		menu.move(1)
	case value >= '1' && value <= '9':
		menu.selectIndex(int(value - '0'))
	case value == 'q' || value == 'Q' || value == 0x03 || value == r.handlers.prefix:
		menu.close()
	}
	return true
}
