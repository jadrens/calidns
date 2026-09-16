package geo

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

// datIndex is a compact, immutable prefix index. In mmap mode, the packed
// records live in a file-backed mapping instead of Go heap slices.
type datIndex struct {
	mu        sync.RWMutex
	codes     []string
	v4        [33][]datV4
	v6        [129][]datV6
	v4Lengths []int
	v6Lengths []int
	mapped    []byte
	mappedV4  [33][]byte
	mappedV6  [129][]byte
}

type datV4 struct {
	address uint32
	code    uint16
}

type datV6 struct {
	address [16]byte
	code    uint16
}

func loadDatIndex(path string, enableMmap bool) (_ *datIndex, err error) {
	var data []byte
	if enableMmap {
		data, err = mapDatFile(path)
		if err != nil {
			return nil, err
		}
		defer func() {
			if unmapErr := syscall.Munmap(data); err == nil && unmapErr != nil {
				err = unmapErr
			}
		}()
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty geoip.dat")
	}

	index := new(datIndex)
	var v4Counts [33]int
	var v6Counts [129]int
	if err := walkDat(data, func(_ string, ip []byte, prefix int) error {
		switch len(ip) {
		case 4:
			if prefix > 32 {
				return fmt.Errorf("invalid IPv4 prefix %d", prefix)
			}
			v4Counts[prefix]++
		case 16:
			if prefix > 128 {
				return fmt.Errorf("invalid IPv6 prefix %d", prefix)
			}
			v6Counts[prefix]++
		default:
			return fmt.Errorf("invalid IP length %d", len(ip))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for prefix, count := range v4Counts {
		if count != 0 {
			index.v4[prefix] = make([]datV4, 0, count)
			index.v4Lengths = append(index.v4Lengths, prefix)
		}
	}
	for prefix, count := range v6Counts {
		if count != 0 {
			index.v6[prefix] = make([]datV6, 0, count)
			index.v6Lengths = append(index.v6Lengths, prefix)
		}
	}
	codeIDs := make(map[string]uint16)
	if err := walkDat(data, func(code string, ip []byte, prefix int) error {
		id, ok := codeIDs[code]
		if !ok {
			if len(index.codes) >= 1<<16 {
				return fmt.Errorf("too many geoip.dat country codes")
			}
			id = uint16(len(index.codes))
			codeIDs[code] = id
			index.codes = append(index.codes, code)
		}
		if len(ip) == 4 {
			address := binary.BigEndian.Uint32(ip)
			if prefix == 0 {
				address = 0
			} else {
				address &= ^uint32(0) << (32 - prefix)
			}
			index.v4[prefix] = append(index.v4[prefix], datV4{address, id})
		} else {
			var address [16]byte
			copy(address[:], ip)
			maskV6(&address, prefix)
			index.v6[prefix] = append(index.v6[prefix], datV6{address, id})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, prefix := range index.v4Lengths {
		entries := index.v4[prefix]
		sort.Slice(entries, func(i, j int) bool { return entries[i].address < entries[j].address })
	}
	for _, prefix := range index.v6Lengths {
		entries := index.v6[prefix]
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].address[:], entries[j].address[:]) < 0 })
	}
	if len(index.codes) == 0 {
		return nil, fmt.Errorf("geoip.dat contains no country prefixes")
	}
	if enableMmap {
		return mapCompactIndex(path, index)
	}
	return index, nil
}

func mapDatFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 || info.Size() > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("invalid geoip.dat size: %d", info.Size())
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// mapCompactIndex writes the already sorted records as 6-byte IPv4 and
// 18-byte IPv6 entries, maps that file, then discards the heap record slices.
// The temporary file is unlinked after mapping and cannot become stale when
// geoip.dat changes; its pages remain file-backed until Close.
func mapCompactIndex(datPath string, heap *datIndex) (*datIndex, error) {
	f, err := os.CreateTemp(filepath.Dir(datPath), ".geoip-index-*")
	if err != nil {
		return nil, fmt.Errorf("creating mmap index next to geoip.dat: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	index := &datIndex{
		codes:     heap.codes,
		v4Lengths: heap.v4Lengths,
		v6Lengths: heap.v6Lengths,
	}
	var v4Offsets [33]int
	var v6Offsets [129]int
	size := 0
	for prefix, records := range heap.v4 {
		v4Offsets[prefix] = size
		size += len(records) * 6
	}
	for prefix, records := range heap.v6 {
		v6Offsets[prefix] = size
		size += len(records) * 18
	}
	if size == 0 {
		return index, nil
	}
	writer := bufio.NewWriterSize(f, 64*1024)
	var v4Record [6]byte
	for _, records := range heap.v4 {
		for _, record := range records {
			binary.BigEndian.PutUint32(v4Record[:4], record.address)
			binary.BigEndian.PutUint16(v4Record[4:], record.code)
			if _, err := writer.Write(v4Record[:]); err != nil {
				return nil, err
			}
		}
	}
	var v6Record [18]byte
	for _, records := range heap.v6 {
		for _, record := range records {
			copy(v6Record[:16], record.address[:])
			binary.BigEndian.PutUint16(v6Record[16:], record.code)
			if _, err := writer.Write(v6Record[:]); err != nil {
				return nil, err
			}
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	index.mapped = data
	for prefix, records := range heap.v4 {
		start := v4Offsets[prefix]
		index.mappedV4[prefix] = data[start : start+len(records)*6]
	}
	for prefix, records := range heap.v6 {
		start := v6Offsets[prefix]
		index.mappedV6[prefix] = data[start : start+len(records)*18]
	}
	return index, nil
}

func (d *datIndex) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mapped == nil {
		return nil
	}
	err := syscall.Munmap(d.mapped)
	d.mapped = nil
	return err
}

func (d *datIndex) lookup(ip string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if addr.Is4() {
		v := binary.BigEndian.Uint32(addr.AsSlice())
		for i := len(d.v4Lengths) - 1; i >= 0; i-- {
			prefix := d.v4Lengths[i]
			key := uint32(0)
			if prefix != 0 {
				key = v & (^uint32(0) << (32 - prefix))
			}
			if d.mapped != nil {
				entries := d.mappedV4[prefix]
				pos := sort.Search(len(entries)/6, func(j int) bool { return binary.BigEndian.Uint32(entries[j*6:]) >= key })
				if pos < len(entries)/6 && binary.BigEndian.Uint32(entries[pos*6:]) == key {
					return d.codes[binary.BigEndian.Uint16(entries[pos*6+4:])]
				}
			} else {
				entries := d.v4[prefix]
				pos := sort.Search(len(entries), func(j int) bool { return entries[j].address >= key })
				if pos < len(entries) && entries[pos].address == key {
					return d.codes[entries[pos].code]
				}
			}
		}
		return ""
	}
	if addr.Is6() {
		v := addr.As16()
		for i := len(d.v6Lengths) - 1; i >= 0; i-- {
			prefix := d.v6Lengths[i]
			key := v
			maskV6(&key, prefix)
			if d.mapped != nil {
				entries := d.mappedV6[prefix]
				pos := sort.Search(len(entries)/18, func(j int) bool { return bytes.Compare(entries[j*18:j*18+16], key[:]) >= 0 })
				if pos < len(entries)/18 && bytes.Equal(entries[pos*18:pos*18+16], key[:]) {
					return d.codes[binary.BigEndian.Uint16(entries[pos*18+16:])]
				}
			} else {
				entries := d.v6[prefix]
				pos := sort.Search(len(entries), func(j int) bool { return bytes.Compare(entries[j].address[:], key[:]) >= 0 })
				if pos < len(entries) && entries[pos].address == key {
					return d.codes[entries[pos].code]
				}
			}
		}
	}
	return ""
}

func maskV6(address *[16]byte, prefix int) {
	full := prefix / 8
	if bits := prefix % 8; bits != 0 {
		address[full] &= byte(0xff << (8 - bits))
		full++
	}
	for i := full; i < len(address); i++ {
		address[i] = 0
	}
}

// walkDat reads the V2Ray geoip.dat protobuf wire format without allocating
// per-CIDR Go objects. Unknown protobuf fields are safely skipped.
func walkDat(data []byte, visit func(code string, ip []byte, prefix int) error) error {
	return walkFields(data, func(field int, wire int, value []byte, _ uint64) error {
		if field != 1 || wire != 2 {
			return nil
		}
		var code string
		if err := walkFields(value, func(field int, wire int, value []byte, _ uint64) error {
			if field == 1 && wire == 2 {
				code = string(value)
			}
			return nil
		}); err != nil {
			return err
		}
		if code == "" {
			return fmt.Errorf("geoip.dat group has no country code")
		}
		// geoip.dat also contains named rule categories such as PRIVATE and
		// CLOUDFLARE. They are not geographic country codes and may overlap
		// country CIDRs, so exclude them from country routing.
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil
		}
		return walkFields(value, func(field int, wire int, value []byte, _ uint64) error {
			if field != 2 || wire != 2 {
				return nil
			}
			var ip []byte
			prefix := -1
			if err := walkFields(value, func(field int, wire int, value []byte, number uint64) error {
				switch {
				case field == 1 && wire == 2:
					ip = value
				case field == 2 && wire == 0:
					if number > 128 {
						return fmt.Errorf("invalid CIDR prefix %d", number)
					}
					prefix = int(number)
				}
				return nil
			}); err != nil {
				return err
			}
			if prefix < 0 {
				return fmt.Errorf("geoip.dat CIDR has no prefix")
			}
			return visit(code, ip, prefix)
		})
	})
}

func walkFields(data []byte, visit func(field int, wire int, value []byte, number uint64) error) error {
	for len(data) != 0 {
		key, n := binary.Uvarint(data)
		if n <= 0 || key>>3 == 0 {
			return fmt.Errorf("invalid protobuf field")
		}
		data = data[n:]
		field, wire := int(key>>3), int(key&7)
		var value []byte
		var number uint64
		switch wire {
		case 0:
			number, n = binary.Uvarint(data)
			if n <= 0 {
				return fmt.Errorf("invalid protobuf varint")
			}
			data = data[n:]
		case 1, 5:
			size := 8
			if wire == 5 {
				size = 4
			}
			if len(data) < size {
				return fmt.Errorf("truncated protobuf field")
			}
			value, data = data[:size], data[size:]
		case 2:
			length, read := binary.Uvarint(data)
			if read <= 0 {
				return fmt.Errorf("invalid protobuf length")
			}
			data = data[read:]
			if length > uint64(len(data)) {
				return fmt.Errorf("truncated protobuf field")
			}
			value, data = data[:int(length)], data[int(length):]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
		if err := visit(field, wire, value, number); err != nil {
			return err
		}
	}
	return nil
}
