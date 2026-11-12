# fleetci's L1 for actions/cache, run on the VM by runScript: the job's
# ACTIONS_RESULTS_URL points here, and every request goes on to the cache
# server it would have used (the L2: GitHub's, or the cache_url server),
# which still decides what each key restores and what a job may save.
#
# The archives travel through here, and a copy of each stays on the job's
# cache disk (/cache/actions). An entry, a key and version, never changes
# once saved, so the next job that restores it reads the disk's copy and
# skips the download. Saving still uploads to the L2.
#
# The disk is a convenience: when it is missing, full or failing, requests
# go on as without it. When the L2 can't be reached, a restore is answered
# from the disk, as the L2 would have chosen among the same entries.
#
# Usage: python3 -I l1cache.py LISTEN_PORT UPSTREAM_URL STORE_DIR [any]
# "any" stores in STORE_DIR whether or not its parent is a mount (tests).
import hashlib, http.client, json, os, re, secrets, shutil, socket, sys, threading, time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
UPSTREAM = sys.argv[2].rstrip("/")
STORE = sys.argv[3]
ANY_STORE = len(sys.argv) > 4 and sys.argv[4] == "any"
SVC = "/twirp/github.actions.results.api.v1.CacheService/"
CHUNK = 1 << 20
HOP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
       "te", "trailers", "transfer-encoding", "upgrade", "host", "content-length"}
# The disk's archives stay under this share of its size, oldest used first out.
SHARE = 0.5
# The L2's own URLs for a download or an upload, by the token of the local
# URL handed out in their place: Azure's SDK adds to an upload URL's query.
LINKS = {}
LINKS_MU = threading.Lock()


def link(*v):
    t = secrets.token_hex(16)
    with LINKS_MU:
        LINKS[t] = v
    return t


def linked(t):
    with LINKS_MU:
        return LINKS.get(t)


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, file=sys.stderr, flush=True)


def store_ok():
    """The store is usable: on a disk of its own, not the VM's root."""
    try:
        parent = os.path.dirname(STORE.rstrip("/"))
        if not ANY_STORE and os.stat(parent).st_dev == os.stat("/").st_dev:
            return False
        os.makedirs(os.path.join(STORE, "tmp"), exist_ok=True)
        return True
    except OSError:
        return False


def entry_id(key, version):
    return hashlib.sha256((version + "\n" + key).encode()).hexdigest()[:32]


def blob_path(eid):
    return os.path.join(STORE, eid + ".blob")


def meta_path(eid):
    return os.path.join(STORE, eid + ".json")


def have(eid):
    return os.path.isfile(blob_path(eid)) and os.path.isfile(meta_path(eid))


def commit(tmp, key, version):
    """Moves a whole archive into the store, then trims the store."""
    eid = entry_id(key, version)
    size = os.path.getsize(tmp)
    with open(meta_path(eid) + ".tmp", "w") as f:
        json.dump({"key": key, "version": version, "size": size, "saved": time.time()}, f)
    os.replace(tmp, blob_path(eid))
    os.replace(meta_path(eid) + ".tmp", meta_path(eid))
    log("kept", key, size, "bytes")
    trim()


def trim():
    try:
        st = os.statvfs(STORE)
        cap = st.f_blocks * st.f_frsize * SHARE
        ents = []
        for n in os.listdir(STORE):
            if n.endswith(".blob"):
                p = os.path.join(STORE, n)
                s = os.stat(p)
                ents.append((s.st_mtime, s.st_size, n[:-5]))
        total = sum(e[1] for e in ents)
        for _, size, eid in sorted(ents):
            if total <= cap:
                break
            for p in (blob_path(eid), meta_path(eid)):
                try:
                    os.remove(p)
                except OSError:
                    pass
            total -= size
    except OSError as e:
        log("trim:", e)


def local_match(key, restore_keys, version):
    """What GitHub would restore, among the disk's entries: the key itself,
    else each restore key in turn, the newest entry it prefixes."""
    try:
        metas = []
        for n in os.listdir(STORE):
            if n.endswith(".json") and have(n[:-5]):
                with open(os.path.join(STORE, n)) as f:
                    m = json.load(f)
                if m.get("version") == version:
                    metas.append(m)
    except (OSError, ValueError):
        return None
    for m in metas:
        if m["key"] == key:
            return m["key"]
    for rk in restore_keys:
        hits = [m for m in metas if m["key"].startswith(rk)]
        if hits:
            return max(hits, key=lambda m: m.get("saved", 0))["key"]
    return None


def connect(url):
    u = urllib.parse.urlsplit(url)
    cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
    conn = cls(u.hostname, u.port, timeout=300)
    path = u.path or "/"
    if u.query:
        path += "?" + u.query
    return conn, path, u.netloc


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    # --- plumbing ---

    def body_chunks(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                size = int(self.rfile.readline().split(b";")[0], 16)
                if size == 0:
                    while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                        pass
                    return
                left = size
                while left:
                    b = self.rfile.read(min(left, CHUNK))
                    if not b:
                        raise ConnectionError("body ended early")
                    left -= len(b)
                    yield b
                self.rfile.readline()
        else:
            left = int(self.headers.get("Content-Length") or 0)
            while left:
                b = self.rfile.read(min(left, CHUNK))
                if not b:
                    raise ConnectionError("body ended early")
                left -= len(b)
                yield b

    def fwd_headers(self, netloc):
        h = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
        h["Host"] = netloc
        return h

    def upstream(self, method, url, body=None, chunks=None):
        """Sends the request on; body is bytes, or chunks a generator."""
        conn, path, netloc = connect(url)
        h = self.fwd_headers(netloc)
        if body is not None:
            h["Content-Length"] = str(len(body))
            conn.request(method, path, body=body, headers=h)
        elif chunks is not None:
            cl = self.headers.get("Content-Length")
            if cl is not None:
                h["Content-Length"] = cl
                conn.putrequest(method, path, skip_host=True, skip_accept_encoding=True)
                for k, v in h.items():
                    conn.putheader(k, v)
                conn.endheaders()
                for c in chunks:
                    conn.send(c)
            else:
                conn.request(method, path, body=chunks, headers=h, encode_chunked=True)
        else:
            conn.request(method, path, headers=h)
        return conn, conn.getresponse()

    def reply(self, status, headers, body):
        self.send_response(status)
        for k, v in headers:
            if k.lower() not in HOP:
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def reply_json(self, status, headers, obj):
        hs = [(k, v) for k, v in headers if k.lower() != "content-type"]
        hs.append(("Content-Type", "application/json"))
        self.reply(status, hs, json.dumps(obj).encode())

    def stream_reply(self, resp, tee=None):
        """Copies the L2's answer to the client, and to tee while it can.
        Returns whether tee got the whole body."""
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP:
                self.send_header(k, v)
        length = resp.getheader("Content-Length")
        if length is not None:
            self.send_header("Content-Length", length)
        else:
            self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        got = 0
        while True:
            b = resp.read(CHUNK)
            if not b:
                break
            got += len(b)
            if tee is not None:
                try:
                    tee.write(b)
                except OSError as e:
                    log("not keeping:", e)
                    tee = None
            if length is not None:
                self.wfile.write(b)
            else:
                self.wfile.write(b"%x\r\n%s\r\n" % (len(b), b))
        if length is None:
            self.wfile.write(b"0\r\n\r\n")
        return tee is not None and (length is None or got == int(length))

    def local_url(self, path):
        host = self.headers.get("Host") or "127.0.0.1:%d" % PORT
        return "http://%s%s" % (host, path)

    # --- routes ---

    def do_GET(self):
        self.route("GET")

    def do_HEAD(self):
        self.route("HEAD")

    def do_POST(self):
        self.route("POST")

    def do_PUT(self):
        self.route("PUT")

    def do_DELETE(self):
        self.route("DELETE")

    def do_PATCH(self):
        self.route("PATCH")

    def route(self, method):
        p = urllib.parse.urlsplit(self.path)
        q = urllib.parse.parse_qs(p.query, keep_blank_values=True)
        if p.path != "/fleetci/ping":
            log(method, p.path[:80], "from", self.client_address[0])
        try:
            if method == "POST" and p.path == SVC + "GetCacheEntryDownloadURL":
                return self.restore()
            if method == "POST" and p.path == SVC + "CreateCacheEntry":
                return self.create()
            if method == "POST" and p.path == SVC + "FinalizeCacheEntryUpload":
                return self.finalize()
            if method == "GET" and p.path == "/fleetci/ping":
                return self.reply(200, [], b"ok")
            if method == "GET" and p.path.startswith("/fleetci/l1/"):
                return self.serve_local(p.path.rsplit("/", 1)[1])
            if method == "GET" and p.path.startswith("/fleetci/fill/"):
                return self.fill(p.path.rsplit("/", 1)[1], q)
            if method == "PUT" and p.path.startswith("/fleetci/put/"):
                return self.put(p.path.rsplit("/", 1)[1], q)
            return self.passthrough(method)
        except (ConnectionError, socket.timeout, OSError, http.client.HTTPException) as e:
            log(method, p.path, "failed:", e)
            try:
                self.reply(502, [], str(e).encode())
            except OSError:
                pass

    def passthrough(self, method):
        has_body = method in ("POST", "PUT", "PATCH") or self.headers.get("Content-Length")
        conn, resp = self.upstream(method, UPSTREAM + self.path,
                                   chunks=self.body_chunks() if has_body else None)
        try:
            self.stream_reply(resp)
        finally:
            conn.close()

    def call(self, body):
        conn, resp = self.upstream("POST", UPSTREAM + self.path, body=body)
        try:
            return resp.status, resp.getheaders(), resp.read()
        finally:
            conn.close()

    def restore(self):
        raw = b"".join(self.body_chunks())
        req = json.loads(raw or b"{}")
        key, version = req.get("key", ""), req.get("version", "")
        rks = req.get("restore_keys") or req.get("restoreKeys") or []
        disk = store_ok()
        try:
            status, headers, body = self.call(raw)
        except (ConnectionError, socket.timeout, OSError, http.client.HTTPException) as e:
            # The L2 is out of reach: the disk answers, if it can.
            m = local_match(key, rks, version) if disk else None
            log("L2 unreachable (%s): restoring %s from the disk" % (e, m) if m else "L2 unreachable: %s" % e)
            if m is None:
                return self.reply(502, [], str(e).encode())
            eid = entry_id(m, version)
            return self.reply_json(200, [], {"ok": True, "matched_key": m,
                                             "signed_download_url": self.local_url("/fleetci/l1/" + eid)})
        if status != 200 or not disk:
            return self.reply(status, headers, body)
        try:
            out = json.loads(body)
        except ValueError:
            return self.reply(status, headers, body)
        m = out.get("matched_key") or out.get("matchedKey")
        url = out.get("signed_download_url") or out.get("signedDownloadUrl")
        if not out.get("ok") or not m or not url:
            return self.reply(status, headers, body)
        eid = entry_id(m, version)
        if have(eid):
            log("hit", m)
            local = self.local_url("/fleetci/l1/" + eid)
        else:
            log("miss", m)
            local = self.local_url("/fleetci/fill/" + link(url, m, version))
        out.pop("signedDownloadUrl", None)
        out["signed_download_url"] = local
        self.reply_json(status, headers, out)

    def serve_local(self, eid):
        try:
            f = open(blob_path(eid), "rb")
        except OSError:
            return self.reply(404, [], b"not on the disk")
        with f:
            size = os.fstat(f.fileno()).st_size
            try:
                os.utime(blob_path(eid))
            except OSError:
                pass
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(size))
            self.end_headers()
            shutil.copyfileobj(f, self.wfile, CHUNK)

    def fill(self, token, q):
        v = linked(token)
        if v is None:
            return self.reply(404, [], b"no such download")
        url, key, version = v
        eid = entry_id(key, version)
        conn, resp = self.upstream("GET", url)
        tmp, tee = None, None
        if resp.status == 200 and store_ok():
            tmp = os.path.join(STORE, "tmp", "%s.%d.fill" % (eid, threading.get_ident()))
            try:
                tee = open(tmp, "wb")
            except OSError as e:
                log("not keeping:", e)
        try:
            whole = self.stream_reply(resp, tee)
        finally:
            conn.close()
            if tee is not None:
                try:
                    tee.close()
                except OSError:
                    whole = False
        if tmp is not None and tee is not None:
            try:
                if whole:
                    commit(tmp, key, version)
                else:
                    os.remove(tmp)
            except OSError as e:
                log("not keeping:", e)

    def create(self):
        raw = b"".join(self.body_chunks())
        req = json.loads(raw or b"{}")
        status, headers, body = self.call(raw)
        if status != 200 or not store_ok():
            return self.reply(status, headers, body)
        try:
            out = json.loads(body)
        except ValueError:
            return self.reply(status, headers, body)
        url = out.get("signed_upload_url") or out.get("signedUploadUrl")
        if not out.get("ok") or not url:
            return self.reply(status, headers, body)
        eid = entry_id(req.get("key", ""), req.get("version", ""))
        shutil.rmtree(os.path.join(STORE, "tmp", eid), ignore_errors=True)
        out.pop("signedUploadUrl", None)
        out["signed_upload_url"] = self.local_url("/fleetci/put/" + link(url, eid))
        self.reply_json(status, headers, out)

    def put(self, token, q):
        """The archive's upload, Azure's way: one PUT, or blocks and then
        their list. Each goes on to the L2, a copy to the disk."""
        v = linked(token)
        if v is None:
            return self.reply(404, [], b"no such upload")
        url, eid = v
        extra = urllib.parse.urlencode({k: v[0] for k, v in q.items()})
        if extra:
            url += ("&" if "?" in url else "?") + extra
        comp = q.get("comp", [""])[0]
        d = os.path.join(STORE, "tmp", eid)
        if comp == "blocklist":
            raw = b"".join(self.body_chunks())
            conn, resp = self.upstream("PUT", url, body=raw)
            try:
                status, headers, body = resp.status, resp.getheaders(), resp.read()
            finally:
                conn.close()
            if status < 300 and os.path.isdir(d):
                ids = [m.group(2) for m in re.finditer(rb"<(Latest|Committed|Uncommitted)>([^<]*)</\1>", raw)]
                try:
                    with open(os.path.join(d, "whole.part"), "wb") as out:
                        for i in ids:
                            with open(os.path.join(d, "block-" + hashlib.sha1(i).hexdigest()), "rb") as f:
                                shutil.copyfileobj(f, out, CHUNK)
                    os.replace(os.path.join(d, "whole.part"), os.path.join(d, "whole"))
                except OSError as e:
                    log("not keeping:", e)
                    shutil.rmtree(d, ignore_errors=True)
            return self.reply(status, headers, body)
        if comp == "block":
            name = "block-" + hashlib.sha1(q.get("blockid", [""])[0].encode()).hexdigest()
        elif comp == "":
            name = "whole"
        else:
            return self.passthrough_to("PUT", url)
        tee = None
        if store_ok():
            try:
                os.makedirs(d, exist_ok=True)
                tee = open(os.path.join(d, name + ".part"), "wb")
            except OSError as e:
                log("not keeping:", e)

        def chunks():
            nonlocal tee
            for c in self.body_chunks():
                if tee is not None:
                    try:
                        tee.write(c)
                    except OSError as e:
                        log("not keeping:", e)
                        tee.close()
                        tee = None
                        shutil.rmtree(d, ignore_errors=True)
                yield c

        conn, resp = self.upstream("PUT", url, chunks=chunks())
        try:
            status, headers, body = resp.status, resp.getheaders(), resp.read()
        finally:
            conn.close()
        if tee is not None:
            try:
                tee.close()
                if status < 300:
                    os.replace(os.path.join(d, name + ".part"), os.path.join(d, name))
            except OSError as e:
                log("not keeping:", e)
        self.reply(status, headers, body)

    def passthrough_to(self, method, url):
        conn, resp = self.upstream(method, url, chunks=self.body_chunks())
        try:
            self.stream_reply(resp)
        finally:
            conn.close()

    def finalize(self):
        raw = b"".join(self.body_chunks())
        req = json.loads(raw or b"{}")
        status, headers, body = self.call(raw)
        key, version = req.get("key", ""), req.get("version", "")
        d = os.path.join(STORE, "tmp", entry_id(key, version))
        try:
            ok = status == 200 and json.loads(body).get("ok")
        except ValueError:
            ok = False
        whole = os.path.join(d, "whole")
        if ok and os.path.isfile(whole):
            try:
                want = int(req.get("size_bytes") or req.get("sizeBytes") or -1)
                if want < 0 or os.path.getsize(whole) == want:
                    commit(whole, key, version)
            except (OSError, ValueError) as e:
                log("not keeping:", e)
        shutil.rmtree(d, ignore_errors=True)
        self.reply(status, headers, body)


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def verify_request(self, request, client_address):
        # Only this VM: loopback, and its containers by Docker's bridge.
        local = request.getsockname()[0]
        return local.startswith("127.") or local in ALLOWED


ALLOWED = set()


def main():
    # The addresses the VM's own containers reach it at: Docker's bridges.
    for line in os.popen("ip -4 -o addr show 2>/dev/null").read().splitlines():
        f = line.split()
        if len(f) > 3 and (f[1] == "docker0" or f[1].startswith("br-")):
            ALLOWED.add(f[3].split("/")[0])
    srv = Server(("0.0.0.0", PORT), Handler)
    log("serving on", PORT, "for", UPSTREAM, "store", STORE)
    srv.serve_forever()


if __name__ == "__main__":
    main()
