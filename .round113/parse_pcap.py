import struct, sys
f = open(sys.argv[1], "rb")
gh = f.read(24)
magic = gh[:4]
if magic == bytes([0xd4, 0xc3, 0xb2, 0xa1]):
    endian = "<"
elif magic == bytes([0xa1, 0xb2, 0xc3, 0xd4]):
    endian = ">"
else:
    print("unknown pcap magic:", magic.hex()); sys.exit(1)
names = {1: "DISCOVER", 2: "OFFER", 3: "REQUEST", 5: "ACK"}
n = 0
while True:
    ph = f.read(16)
    if len(ph) < 16: break
    ts, tus, caplen, wirelen = struct.unpack(endian + "IIII", ph)
    pkt = f.read(caplen)
    n += 1
    if len(pkt) < 34: continue
    eth_type = struct.unpack("!H", pkt[12:14])[0]
    if eth_type == 0x0806:
        a = pkt[14:]
        spa = ".".join(str(b) for b in a[14:18]); tpa = ".".join(str(b) for b in a[24:28])
        op = struct.unpack("!H", a[6:8])[0]
        print("pkt%02d ARP op=%d %s -> who-has %s" % (n, op, spa, tpa)); continue
    if eth_type != 0x0800:
        print("pkt%02d eth type=0x%x (non-ip)" % (n, eth_type)); continue
    ip = pkt[14:]
    ihl = (ip[0] & 0xf) * 4
    proto = ip[9]
    src = ".".join(str(b) for b in ip[12:16]); dst = ".".join(str(b) for b in ip[16:20])
    if proto != 17:
        print("pkt%02d %s->%s proto=%d" % (n, src, dst, proto)); continue
    udp = ip[ihl:]
    sport, dport = struct.unpack("!HH", udp[:4])
    if 67 not in (sport, dport) and 68 not in (sport, dport):
        print("pkt%02d %s:%d->%s:%d udp" % (n, src, sport, dst, dport)); continue
    payload = udp[8:]
    giaddr = ".".join(str(b) for b in payload[12:16]) if len(payload) >= 240 else "?"
    chaddr = ":".join("%02x" % b for b in payload[28:34]) if len(payload) >= 240 else "?"
    ck = payload.find(bytes([99, 130, 83, 99]), 236)
    opts = payload[ck:] if ck >= 0 else b""
    mtype = "?"
    if ck >= 0:
        i = 4
        while i + 2 <= len(opts):
            c = opts[i]
            if c == 255: break
            ln = opts[i+1]
            if c == 53: mtype = names.get(opts[i+2], opts[i+2])
            i += 2 + ln
    print("pkt%02d %s: %s:%d -> %s:%d  giaddr=%s  chaddr=%s" % (n, mtype, src, sport, dst, dport, giaddr, chaddr))
