#!/usr/bin/env python3
"""Isolated Linux/FRR integration. Requires root, iproute2, nft, FRR, dnspython.

No host forwarding/firewall configuration is modified. All addresses, daemons,
routes and packets live inside four temporary network namespaces. Logs survive.
"""
import asyncio
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


async def fixtures(log_path):
    import dns.message
    import dns.rrset
    import dns.rdatatype

    class DNS(asyncio.DatagramProtocol):
        def connection_made(self, transport):
            self.transport = transport

        def datagram_received(self, packet, address):
            q = dns.message.from_wire(packet)
            r = dns.message.make_response(q)
            kind = q.question[0].rdtype
            ttl = 60 if str(q.question[0].name).startswith("persistent.") else 2
            if kind == dns.rdatatype.A:
                r.answer.append(dns.rrset.from_text(q.question[0].name, ttl, "IN", "A", "11.0.0.2"))
            elif kind == dns.rdatatype.AAAA:
                r.answer.append(dns.rrset.from_text(q.question[0].name, ttl, "IN", "AAAA", "2001:4860:ffff::2"))
            elif kind == dns.rdatatype.NS:
                r.answer.append(dns.rrset.from_text(q.question[0].name, 60, "IN", "NS", "ns.test."))
            self.transport.sendto(r.to_wire(), address)

    async def http(reader, writer):
        try:
            header = await reader.readuntil(b"\r\n\r\n")
            if b" /slow " in header:
                await asyncio.sleep(6)
            body = b"bgp-bypass-ok"
            if b" /large " in header:
                body *= 4096
            writer.write(f"HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: {len(body)}\r\n\r\n".encode() + body)
            await writer.drain()
        finally:
            writer.close()

    async def socks(reader, writer):
        remote = None
        udp = None
        try:
            greeting = await reader.readexactly(2)
            await reader.readexactly(greeting[1])
            writer.write(b"\x05\x00")
            await writer.drain()
            request = await reader.readexactly(4)
            if request[0] != 5 or request[1] not in (1, 3):
                return
            kind = request[3]
            if kind == 1:
                host = socket.inet_ntop(socket.AF_INET, await reader.readexactly(4))
            elif kind == 4:
                host = socket.inet_ntop(socket.AF_INET6, await reader.readexactly(16))
            elif kind == 3:
                size = (await reader.readexactly(1))[0]
                host = (await reader.readexactly(size)).decode()
            else:
                return
            port = int.from_bytes(await reader.readexactly(2), "big")
            if request[1] == 3:
                udp, _ = await asyncio.get_running_loop().create_datagram_endpoint(SocksUDP, local_addr=("0.0.0.0", 0))
                relay_port = udp.get_extra_info("sockname")[1]
                writer.write(b"\x05\x00\x00\x01" + socket.inet_aton("11.0.0.2") + relay_port.to_bytes(2, "big"))
                await writer.drain()
                await reader.read()
                return
            upstream, remote = await asyncio.open_connection(host, port)
            with open(log_path, "a") as out:
                out.write(f"{host}:{port}\n")
            writer.write(b"\x05\x00\x00\x01\x00\x00\x00\x00\x00\x00")
            await writer.drain()

            async def copy(source, target):
                while data := await source.read(16384):
                    target.write(data)
                    await target.drain()

            tasks = [asyncio.create_task(copy(reader, remote)), asyncio.create_task(copy(upstream, writer))]
            _, pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
            for task in pending:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
        except (OSError, asyncio.IncompleteReadError):
            pass
        finally:
            if remote:
                remote.close()
            if udp:
                udp.close()
            writer.close()

    class Echo(asyncio.DatagramProtocol):
        def connection_made(self, transport):
            self.transport = transport

        def datagram_received(self, packet, address):
            self.transport.sendto(packet, address)

    class SocksUDP(asyncio.DatagramProtocol):
        def connection_made(self, transport):
            self.transport = transport

        def datagram_received(self, packet, address):
            asyncio.create_task(self.relay(packet, address))

        async def relay(self, packet, address):
            if len(packet) < 10 or packet[:3] != b"\0\0\0" or packet[3] not in (1, 4):
                return
            family = socket.AF_INET if packet[3] == 1 else socket.AF_INET6
            size = 4 if packet[3] == 1 else 16
            host = socket.inet_ntop(family, packet[4:4+size])
            port = int.from_bytes(packet[4+size:6+size], "big")
            sock = socket.socket(family, socket.SOCK_DGRAM)
            sock.setblocking(False)
            try:
                loop = asyncio.get_running_loop()
                await loop.sock_sendto(sock, packet[6+size:], (host, port))
                result, _ = await asyncio.wait_for(loop.sock_recvfrom(sock, 65535), 2)
                with open(log_path, "a") as out:
                    out.write(f"udp {host}:{port}\n")
                self.transport.sendto(packet[:6+size] + result, address)
            finally:
                sock.close()

    loop = asyncio.get_running_loop()
    await loop.create_datagram_endpoint(DNS, local_addr=("0.0.0.0", 5353))
    await loop.create_datagram_endpoint(Echo, local_addr=("0.0.0.0", 8001))
    await loop.create_datagram_endpoint(Echo, local_addr=("::", 8001))
    servers = [await asyncio.start_server(http, "0.0.0.0", 8000),
               await asyncio.start_server(http, "::", 8000),
               await asyncio.start_server(socks, "0.0.0.0", 1080)]
    await asyncio.gather(*(server.serve_forever() for server in servers))


def main(binary):
    if sys.platform != "linux" or os.geteuid() != 0:
        raise SystemExit("requires Linux root; this test has NOT run")
    for tool in ("ip", "nft", "/usr/lib/frr/zebra", "/usr/lib/frr/bgpd"):
        if not shutil.which(tool):
            raise SystemExit(f"missing {tool}; this test has NOT run")
    work = Path(tempfile.mkdtemp(prefix="bgp-network-"))
    work.chmod(0o755)
    logs = work / "logs"
    logs.mkdir(mode=0o777)
    logs.chmod(0o777)
    frr = work / "frr"
    frr.mkdir(mode=0o777)
    frr.chmod(0o777)
    script = str(Path(__file__).resolve())
    namespaces = {role: f"bgp-{os.getpid()}-{role}" for role in ("c", "r", "s", "o")}
    created = []
    processes = []

    def cmd(*args, role=None, **kwargs):
        argv = list(args)
        if role:
            argv = ["ip", "netns", "exec", namespaces[role], *argv]
        return subprocess.run(argv, check=True, text=True, capture_output=True, **kwargs).stdout

    def spawn(role, label, *args):
        stream = open(logs / (label + ".log"), "w")
        process = subprocess.Popen(["ip", "netns", "exec", namespaces[role], *args], stdout=stream, stderr=subprocess.STDOUT)
        stream.close()
        processes.append(process)
        return process

    def wait_for(description, predicate, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                print("PASS", description, flush=True)
                return
            time.sleep(.1)
        raise AssertionError(description)

    def route_present(ip):
        family = "-6" if ":" in ip else "-4"
        return "proto bgp" in cmd("ip", family, "route", "show", "exact", ip, role="r")

    def query(kind, name="proxy.test."):
        return cmd("python3", "-c", f"import dns.message,dns.query; print(dns.query.udp(dns.message.make_query('{name}', '{kind}'), '192.168.66.2', timeout=3))", role="c")

    def get_udp(ip):
        family = "AF_INET6" if ":" in ip else "AF_INET"
        return cmd("python3", "-c", f"import socket; s=socket.socket(socket.{family},socket.SOCK_DGRAM); s.settimeout(3); s.sendto(b'udp-bypass-ok',('{ip}',8001)); assert s.recv(100)==b'udp-bypass-ok'", role="c")

    def get(ip, path="/"):
        url = f"http://[{ip}]:8000{path}" if ":" in ip else f"http://{ip}:8000{path}"
        return cmd("python3", "-c", "import urllib.request; print(urllib.request.build_opener(urllib.request.ProxyHandler({})).open('" + url + "', timeout=10).read().decode())", role="c")

    try:
        for ns in namespaces.values():
            cmd("ip", "netns", "add", ns)
            created.append(ns)
            cmd("ip", "-n", ns, "link", "set", "lo", "up")
        links = [("c", "r", "10.10.0.2/24", "10.10.0.1/24", "fd10::2/64", "fd10::1/64"),
                 ("s", "r", "192.168.66.2/24", "192.168.66.1/24", "fd66::2/64", "fd66::1/64"),
                 ("o", "r", "11.0.0.2/24", "11.0.0.1/24", "2001:4860:ffff::2/64", "2001:4860:ffff::1/64")]
        for a, b, a4, b4, a6, b6 in links:
            left, right = f"{a}-{b}", f"{b}-{a}"
            cmd("ip", "link", "add", left, "netns", namespaces[a], "type", "veth", "peer", "name", right, "netns", namespaces[b])
            for role, dev, ip4, ip6 in ((a, left, a4, a6), (b, right, b4, b6)):
                cmd("ip", "address", "add", ip4, "dev", dev, role=role)
                cmd("ip", "-6", "address", "add", ip6, "dev", dev, "nodad", role=role)
                cmd("ip", "link", "set", dev, "up", role=role)
                cmd("sysctl", "-qw", f"net.ipv4.conf.{dev}.rp_filter=0", role=role)
        for role, gateway4, gateway6 in (("c", "10.10.0.1", "fd10::1"), ("s", "192.168.66.1", "fd66::1"), ("o", "11.0.0.1", "2001:4860:ffff::1")):
            cmd("ip", "route", "add", "default", "via", gateway4, role=role)
            cmd("ip", "-6", "route", "add", "default", "via", gateway6, role=role)
        for role in ("r", "s"):
            cmd("sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv6.conf.all.forwarding=1", "net.ipv4.conf.all.rp_filter=0", role=role)
        for family, prefix in (("-4", "11.0.0.0/24"), ("-6", "2001:4860:ffff::/64")):
            cmd("ip", family, "route", "add", prefix, "dev", "r-o", "table", "100", role="r")
            cmd("ip", family, "rule", "add", "iif", "r-s", "lookup", "100", role="r")
            cmd("ip", family, "rule", "add", "fwmark", "2", "lookup", "100", role="s")
            cmd("ip", family, "route", "add", "local", "default", "dev", "lo", "table", "100", role="s")
        nft = '''table inet bypass_test {
 chain prerouting { type filter hook prerouting priority mangle; policy accept;
  ct direction reply return
  tcp dport { 179, 9098, 9099 } return
  iifname "s-r" meta nfproto ipv4 meta l4proto { tcp, udp } tproxy to :12345 meta mark set 2 accept
  iifname "s-r" meta nfproto ipv6 meta l4proto { tcp, udp } tproxy to :12346 meta mark set 2 accept
 }
}'''
        cmd("nft", "-f", "-", role="s", input=nft)
        zebra_conf = work / "zebra.conf"
        zebra_conf.write_text("hostname bgp-test\n")
        bgp_conf = work / "bgpd.conf"
        bgp_conf.write_text("""router bgp 65001
 bgp router-id 192.168.66.1
 no bgp ebgp-requires-policy
 no bgp default ipv4-unicast
 neighbor 192.168.66.2 remote-as 65002
 neighbor fd66::2 remote-as 65002
 address-family ipv4 unicast
  neighbor 192.168.66.2 activate
 exit-address-family
 address-family ipv6 unicast
  neighbor fd66::2 activate
 exit-address-family
""")
        for daemon, conf in (("zebra", zebra_conf), ("bgpd", bgp_conf)):
            spawn("r", daemon, f"/usr/lib/frr/{daemon}", "-f", str(conf), "-i", str(frr / (daemon + ".pid")), "-z", str(frr / "zserv.api"), "--vty_socket", str(frr), "--log", "stdout")
            time.sleep(.5)
        cmd("sysctl", "-qw", "net.ipv6.bindv6only=1", role="o")
        fixture = spawn("o", "fixtures", "python3", script, "--fixtures", str(logs / "socks-connections.log"))
        config = json.loads(Path("docs/examples/bgp-bypass.json").read_text())
        config["dns"] = {"servers": [{"type": "udp", "tag": "test", "server": "11.0.0.2", "server_port": 5353}], "final": "test"}
        config["outbounds"] = [config["outbounds"][0], {"type": "socks", "tag": "proxy-default", "server": "11.0.0.2", "server_port": 1080}]
        config["route"]["rules"] = [config["route"]["rules"][0],
            {"inbound": ["lan-tproxy", "lan-tproxy6"], "bgp_bypass": "bypass-control", "outbound": "proxy-default"},
            {"domain_suffix": ["test"], "outbound": "proxy-default"}]
        service = config["services"][0]
        service.update(advertise=True, state_path=str(work / "state.json"))
        service["timing"].update(withdraw_delay="1s", settle_delay="0s")
        config["observability"].update(health_listen="192.168.66.2")
        path = work / "config.json"
        path.write_text(json.dumps(config))
        app = spawn("s", "sing-box", binary, "run", "-c", str(path))
        wait_for("DNS and BGP readiness", lambda: subprocess.run(["ip", "netns", "exec", namespaces["c"], "python3", "-c", "import urllib.request; urllib.request.urlopen('http://192.168.66.2:9099/services/bgp-bypass/bypass-control/readyz',timeout=1)"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0)
        # Cold DNS, convergence and actual transparent proxying in both families.
        for kind, ip in (("A", "11.0.0.2"), ("AAAA", "2001:4860:ffff::2")):
            query(kind)
            wait_for(f"{kind} route installed in FRR/kernel", lambda: route_present(ip))
            assert "bgp-bypass-ok" in get(ip)
            assert ip in (logs / "socks-connections.log").read_text()
            wait_for(f"{kind} TTL withdrawal", lambda: not route_present(ip), 10)
        # An established TCP stream pins the route after DNS TTL expiration.
        query("A")
        wait_for("route before long connection", lambda: route_present("11.0.0.2"))
        slow = spawn("c", "long-connection", "python3", "-c", "import urllib.request; print(urllib.request.urlopen('http://11.0.0.2:8000/slow',timeout=10).read())")
        time.sleep(4)
        assert route_present("11.0.0.2"), "active connection lost its route"
        assert slow.wait(timeout=10) == 0
        wait_for("route withdrawn after connection close", lambda: not route_present("11.0.0.2"), 10)
        # UDP sessions retain their route until the upstream idle timeout. Use
        # a long lease for the remaining scenarios so that check is separate.
        for kind, ip in (("A", "11.0.0.2"), ("AAAA", "2001:4860:ffff::2")):
            query(kind, "persistent.test.")
            wait_for(f"{kind} route before UDP", lambda: route_present(ip))
            get_udp(ip)
            assert f"udp {ip}:8001" in (logs / "socks-connections.log").read_text()
        # Verify bulk TCP at IPv6 minimum link MTU, without blocking ICMP errors.
        cmd("ip", "link", "set", "r-o", "mtu", "1280", role="r")
        cmd("ip", "link", "set", "o-r", "mtu", "1280", role="o")
        for ip in ("11.0.0.2", "2001:4860:ffff::2"):
            assert len(get(ip, "/large").strip()) == len("bgp-bypass-ok") * 4096
        # Wait for the async atomic snapshot, then cleanly restart. Runtime
        # connection references must not be restored from the file.
        wait_for("state persisted", lambda: (work / "state.json").exists() and "persistent.test" in (work / "state.json").read_text(), 10)
        app.terminate()
        app.wait(timeout=10)
        wait_for("normal shutdown withdrawal", lambda: not route_present("11.0.0.2"))
        app = spawn("s", "sing-box-restart", binary, "run", "-c", str(path))
        wait_for("restart restores dual-stack leases", lambda: route_present("11.0.0.2") and route_present("2001:4860:ffff::2"))
        # Simulated abrupt process failure must not leave stale BGP routes.
        query("A")
        wait_for("route before crash", lambda: route_present("11.0.0.2"))
        app.kill()
        app.wait(timeout=5)
        wait_for("crash withdrawal", lambda: not route_present("11.0.0.2"), 12)
        assert "bgp-bypass-ok" in get("11.0.0.2"), "direct fallback failed"
        assert fixture.poll() is None
        print("PASS namespace integration; RouterOS and performance acceptance remain separate", flush=True)
    finally:
        for role in ("r", "s"):
            if namespaces[role] in created:
                for family in ("-4", "-6"):
                    try:
                        (logs / f"{role}{family}-routes.log").write_text(cmd("ip", family, "route", "show", "table", "all", role=role))
                    except subprocess.CalledProcessError:
                        pass
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
        for process in reversed(processes):
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for ns in reversed(created):
            subprocess.run(["ip", "netns", "delete", ns], check=False)
        print("Diagnostics:", logs, flush=True)


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--fixtures":
        asyncio.run(fixtures(sys.argv[2]))
    elif len(sys.argv) == 2:
        main(str(Path(sys.argv[1]).resolve()))
    else:
        raise SystemExit("usage: bgp_network.py /path/to/sing-box-with-bgp")
