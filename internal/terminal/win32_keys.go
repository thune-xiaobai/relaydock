package terminal

import (
	"strconv"
	"strings"
)

// ConPTY requests Win32 keyboard encoding with CSI ? 9001 h. Decode only the
// small part needed by the local escape; forward every other event unchanged.
// https://github.com/microsoft/terminal/blob/main/doc/specs/%234999%20-%20Improved%20keyboard%20handling%20in%20Conpty.md
type win32Key struct {
	char    int
	passive bool
}

func parseWin32Key(packet []byte) (win32Key, bool) {
	if len(packet) < 3 || packet[0] != '\x1b' || packet[1] != '[' || packet[len(packet)-1] != '_' {
		return win32Key{}, false
	}
	fields := strings.Split(string(packet[2:len(packet)-1]), ";")
	if len(fields) > 6 {
		return win32Key{}, false
	}
	values := [6]uint64{0, 0, 0, 0, 0, 1}
	for i, field := range fields {
		if field == "" {
			continue
		}
		limit := 16
		if i == 4 {
			limit = 32
		}
		value, err := strconv.ParseUint(field, 10, limit)
		if err != nil {
			return win32Key{}, false
		}
		values[i] = value
	}
	if values[3] > 1 {
		return win32Key{}, false
	}
	vk, char := values[0], int(values[2])
	modifier := vk == 0x10 || vk == 0x11 || vk == 0x12 || vk >= 0xa0 && vk <= 0xa5
	passive := values[3] == 0 || values[5] == 0 || char == 0 && modifier
	// Preserve modified and auto-repeated punctuation as keyboard events; only
	// a single literal tilde/dot can participate in the local escape.
	if (char == '~' || char == '.') && (values[4]&0xf != 0 || values[5] != 1) {
		char = -1
	}
	return win32Key{char: char, passive: passive}, true
}
