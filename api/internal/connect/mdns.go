package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

const (
	MDNSServiceType = "_kamui-cloud._tcp.local."
	mdnsMulticast   = "224.0.0.251:5353"

	dnsTypeA   uint16 = 1
	dnsTypePTR uint16 = 12
	dnsTypeTXT uint16 = 16
	dnsTypeSRV uint16 = 33
)

type MDNSService struct {
	Instance string            `json:"instance"`
	Host     string            `json:"host"`
	Address  string            `json:"address"`
	Port     int               `json:"port"`
	TXT      map[string]string `json:"txt,omitempty"`
}

func encodeDNSName(name string) ([]byte, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(name), ".")
	if trimmed == "" {
		return []byte{0}, nil
	}
	var out bytes.Buffer
	for label := range strings.SplitSeq(trimmed, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS label %q", label)
		}
		out.WriteByte(byte(len(label)))
		out.WriteString(label)
	}
	out.WriteByte(0)
	return out.Bytes(), nil
}

func decodeDNSName(pkt []byte, offset int) (string, int, error) {
	var labels []string
	jumped := false
	origNext := 0
	seen := 0
	for {
		if offset >= len(pkt) {
			return "", 0, errors.New("truncated DNS name")
		}
		seen++
		if seen > 64 {
			return "", 0, errors.New("DNS compression loop")
		}
		l := int(pkt[offset])
		if l == 0 {
			if !jumped {
				origNext = offset + 1
			}
			break
		}
		if l&0xC0 == 0xC0 {
			if offset+1 >= len(pkt) {
				return "", 0, errors.New("truncated DNS pointer")
			}
			ptr := int(l&0x3F)<<8 | int(pkt[offset+1])
			if !jumped {
				origNext = offset + 2
			}
			jumped = true
			offset = ptr
			continue
		}
		offset++
		if offset+l > len(pkt) {
			return "", 0, errors.New("truncated DNS label")
		}
		labels = append(labels, string(pkt[offset:offset+l]))
		offset += l
	}
	return strings.Join(labels, ".") + ".", origNext, nil
}

func BuildMDNSQueryPacket(serviceType string) ([]byte, error) {
	if serviceType == "" {
		serviceType = MDNSServiceType
	}
	qname, err := encodeDNSName(serviceType)
	if err != nil {
		return nil, err
	}
	var pkt bytes.Buffer
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[4:6], 1) // QDCOUNT = 1
	pkt.Write(hdr[:])
	pkt.Write(qname)
	_ = binary.Write(&pkt, binary.BigEndian, dnsTypePTR)
	_ = binary.Write(&pkt, binary.BigEndian, uint16(1)) // IN
	return pkt.Bytes(), nil
}

func BuildMDNSResponsePacket(svc MDNSService) ([]byte, error) {
	instanceFQDN := svc.Instance + "." + MDNSServiceType
	hostFQDN := strings.TrimSuffix(svc.Host, ".") + ".local."
	if strings.HasSuffix(svc.Host, ".local.") || strings.HasSuffix(svc.Host, ".local") {
		hostFQDN = strings.TrimSuffix(svc.Host, ".") + "."
	}
	ip, err := netip.ParseAddr(svc.Address)
	if err != nil || !ip.Is4() {
		return nil, errors.New("mDNS IPv4 address required")
	}

	var pkt bytes.Buffer
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[2:4], 0x8400) // Response + Authoritative
	binary.BigEndian.PutUint16(hdr[6:8], 4)      // ANCOUNT = 4 (PTR, SRV, TXT, A)
	pkt.Write(hdr[:])

	writeRR := func(name string, rrType uint16, ttl uint32, rdata []byte) error {
		encodedName, err := encodeDNSName(name)
		if err != nil {
			return err
		}
		pkt.Write(encodedName)
		_ = binary.Write(&pkt, binary.BigEndian, rrType)
		_ = binary.Write(&pkt, binary.BigEndian, uint16(1))
		_ = binary.Write(&pkt, binary.BigEndian, ttl)
		_ = binary.Write(&pkt, binary.BigEndian, uint16(len(rdata)))
		pkt.Write(rdata)
		return nil
	}

	ptrData, err := encodeDNSName(instanceFQDN)
	if err != nil {
		return nil, err
	}
	if err := writeRR(MDNSServiceType, dnsTypePTR, 120, ptrData); err != nil {
		return nil, err
	}

	var srvBuf bytes.Buffer
	_ = binary.Write(&srvBuf, binary.BigEndian, uint16(0))
	_ = binary.Write(&srvBuf, binary.BigEndian, uint16(0))
	_ = binary.Write(&srvBuf, binary.BigEndian, uint16(svc.Port))
	hostData, err := encodeDNSName(hostFQDN)
	if err != nil {
		return nil, err
	}
	srvBuf.Write(hostData)
	if err := writeRR(instanceFQDN, dnsTypeSRV, 120, srvBuf.Bytes()); err != nil {
		return nil, err
	}

	var txtBuf bytes.Buffer
	for k, v := range svc.TXT {
		entry := k + "=" + v
		if len(entry) > 255 {
			entry = entry[:255]
		}
		txtBuf.WriteByte(byte(len(entry)))
		txtBuf.WriteString(entry)
	}
	if txtBuf.Len() == 0 {
		txtBuf.WriteByte(0)
	}
	if err := writeRR(instanceFQDN, dnsTypeTXT, 120, txtBuf.Bytes()); err != nil {
		return nil, err
	}

	a4 := ip.As4()
	if err := writeRR(hostFQDN, dnsTypeA, 120, a4[:]); err != nil {
		return nil, err
	}
	return pkt.Bytes(), nil
}

func ParseMDNSResponse(pkt []byte) ([]MDNSService, error) {
	if len(pkt) < 12 {
		return nil, errors.New("short DNS packet")
	}
	flags := binary.BigEndian.Uint16(pkt[2:4])
	if flags&0x8000 == 0 {
		return nil, nil // Query, not a response.
	}
	qd := int(binary.BigEndian.Uint16(pkt[4:6]))
	an := int(binary.BigEndian.Uint16(pkt[6:8]))
	ns := int(binary.BigEndian.Uint16(pkt[8:10]))
	ar := int(binary.BigEndian.Uint16(pkt[10:12]))

	off := 12
	for range qd {
		_, next, err := decodeDNSName(pkt, off)
		if err != nil {
			return nil, err
		}
		off = next + 4
		if off > len(pkt) {
			return nil, errors.New("truncated question")
		}
	}

	byInstance := map[string]*MDNSService{}
	hosts := map[string]string{}
	totalRRs := an + ns + ar
	for range totalRRs {
		if off >= len(pkt) {
			break
		}
		name, next, err := decodeDNSName(pkt, off)
		if err != nil {
			return nil, err
		}
		if next+10 > len(pkt) {
			return nil, errors.New("truncated RR header")
		}
		rrType := binary.BigEndian.Uint16(pkt[next : next+2])
		rdLen := int(binary.BigEndian.Uint16(pkt[next+8 : next+10]))
		rdataStart := next + 10
		rdataEnd := rdataStart + rdLen
		if rdataEnd > len(pkt) {
			return nil, errors.New("truncated RR data")
		}
		rdata := pkt[rdataStart:rdataEnd]
		off = rdataEnd

		switch rrType {
		case dnsTypePTR:
			if strings.EqualFold(name, MDNSServiceType) {
				target, _, err := decodeDNSName(pkt, rdataStart)
				if err == nil {
					inst := strings.TrimSuffix(target, "."+MDNSServiceType)
					if byInstance[target] == nil {
						byInstance[target] = &MDNSService{Instance: inst, TXT: map[string]string{}}
					}
				}
			}
		case dnsTypeSRV:
			if len(rdata) >= 6 {
				port := int(binary.BigEndian.Uint16(rdata[4:6]))
				target, _, err := decodeDNSName(pkt, rdataStart+6)
				if err == nil {
					svc := byInstance[name]
					if svc == nil {
						svc = &MDNSService{Instance: strings.TrimSuffix(name, "."+MDNSServiceType), TXT: map[string]string{}}
						byInstance[name] = svc
					}
					svc.Port = port
					svc.Host = target
				}
			}
		case dnsTypeTXT:
			svc := byInstance[name]
			if svc == nil {
				svc = &MDNSService{Instance: strings.TrimSuffix(name, "."+MDNSServiceType), TXT: map[string]string{}}
				byInstance[name] = svc
			}
			for i := 0; i < len(rdata); {
				l := int(rdata[i])
				i++
				if i+l > len(rdata) {
					break
				}
				kv := string(rdata[i : i+l])
				i += l
				if k, v, ok := strings.Cut(kv, "="); ok {
					svc.TXT[k] = v
				}
			}
		case dnsTypeA:
			if len(rdata) == 4 {
				hosts[strings.ToLower(name)] = net.IP(rdata).String()
			}
		}
	}

	out := make([]MDNSService, 0, len(byInstance))
	for _, svc := range byInstance {
		if svc.Host != "" && svc.Address == "" {
			svc.Address = hosts[strings.ToLower(svc.Host)]
		}
		if svc.Port > 0 && svc.Address != "" {
			out = append(out, *svc)
		}
	}
	return out, nil
}

func DiscoverMDNS(ctx context.Context, timeout time.Duration) ([]MDNSService, error) {
	if timeout <= 0 {
		timeout = 1200 * time.Millisecond
	}
	maddr, err := net.ResolveUDPAddr("udp4", mdnsMulticast)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	query, err := BuildMDNSQueryPacket(MDNSServiceType)
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(query, maddr); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	seen := map[string]MDNSService{}
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		services, err := ParseMDNSResponse(buf[:n])
		if err != nil {
			continue
		}
		for _, svc := range services {
			seen[fmt.Sprintf("%s:%d", svc.Address, svc.Port)] = svc
		}
	}
	out := make([]MDNSService, 0, len(seen))
	for _, svc := range seen {
		out = append(out, svc)
	}
	return out, nil
}

func StartMDNSResponder(ctx context.Context, svc MDNSService) error {
	maddr, err := net.ResolveUDPAddr("udp4", mdnsMulticast)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, maddr)
	if err != nil {
		return err
	}
	pkt, err := BuildMDNSResponsePacket(svc)
	if err != nil {
		_ = conn.Close()
		return err
	}
	go func() {
		defer conn.Close()
		unwatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer unwatch()
		buf := make([]byte, 2048)
		for ctx.Err() == nil {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n >= 12 && binary.BigEndian.Uint16(buf[2:4])&0x8000 == 0 && bytes.Contains(buf[:n], []byte("kamui-cloud")) {
				_, _ = conn.WriteToUDP(pkt, maddr)
				if src != nil {
					_, _ = conn.WriteToUDP(pkt, src)
				}
			}
		}
	}()
	return nil
}
