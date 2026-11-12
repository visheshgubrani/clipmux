// src/token.ts
var ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
var INDEX = new Map([...ALPHABET].map((c, i) => [c, i]));
function b64urlDecode(s) {
  if (s.length % 4 === 1) return null;
  const out = new Uint8Array(Math.floor(s.length * 6 / 8));
  let acc = 0;
  let bits = 0;
  let o = 0;
  for (const c of s) {
    const v = INDEX.get(c);
    if (v === void 0) return null;
    acc = acc << 6 | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = acc >> bits & 255;
    }
  }
  if (bits > 0 && (acc & (1 << bits) - 1) !== 0) return null;
  return out;
}
function object(part) {
  const bytes = b64urlDecode(part);
  if (!bytes) return null;
  let raw;
  try {
    raw = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(bytes);
  } catch {
    return null;
  }
  let obj;
  try {
    obj = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof obj !== "object" || obj === null || Array.isArray(obj)) return null;
  return { obj, raw };
}
var str = (o, name) => {
  const v = o[name];
  return typeof v === "string" ? v : "";
};
function integer(o, raw, name) {
  if (!(name in o)) return null;
  const v = o[name];
  if (v === null) return 0;
  if (typeof v !== "number" || !Number.isSafeInteger(v)) return null;
  const re = new RegExp(`"${name}"\\s*:\\s*(-?[0-9][0-9.eE+-]*)`, "g");
  let literal = "";
  for (const m of raw.matchAll(re)) literal = m[1] ?? "";
  return /^-?(0|[1-9][0-9]*)$/.test(literal) ? v : null;
}
function parse(token) {
  const parts = token.split(".");
  if (parts.length !== 3 || token.length > 4096) return null;
  const [h, p, s] = parts;
  const hdr = object(h);
  if (!hdr || str(hdr.obj, "alg") !== "ES256" || str(hdr.obj, "kid") === "") return null;
  const payload = object(p);
  if (!payload) return null;
  const iat = integer(payload.obj, payload.raw, "iat");
  const exp = integer(payload.obj, payload.raw, "exp");
  if (iat === null || exp === null) return null;
  const sig = b64urlDecode(s);
  if (sig?.length !== 64) return null;
  return {
    kid: str(hdr.obj, "kid"),
    claims: {
      sub: str(payload.obj, "sub"),
      aud: str(payload.obj, "aud"),
      iat,
      exp,
      vid: str(payload.obj, "vid")
    },
    input: `${h}.${p}`,
    sig
  };
}
async function verify(t, jwk) {
  try {
    const key = await crypto.subtle.importKey(
      "jwk",
      { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y, ext: true },
      { name: "ECDSA", namedCurve: "P-256" },
      false,
      ["verify"]
    );
    return await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      key,
      t.sig,
      new TextEncoder().encode(t.input)
    );
  } catch {
    return false;
  }
}

// src/delivery.ts
var EDGE_HEADER = "X-ClipMux-Edge";
var SEGMENT_SECONDS = 6;
var CONTENT_TYPES = {
  ".m3u8": "application/vnd.apple.mpegurl",
  ".mp4": "video/mp4",
  ".m4s": "video/iso.segment",
  ".jpg": "image/jpeg"
};
var TRACK_PATH = /^(trk_[0-9a-z_]{1,64})\.(m3u8|vtt)$/;
var OUTPUT_PATH = /^(manifest\.json|master\.m3u8|poster\.jpg|([1-9][0-9]{1,3}p|audio)\/(index\.m3u8|init\.mp4|seg_[0-9]{5,6}\.m4s))$/;
var CLOCK_SKEW = 60;
var IMMUTABLE = "max-age=31536000, immutable";
function common(h) {
  h.set("Access-Control-Allow-Origin", "*");
  h.set("Access-Control-Expose-Headers", "Content-Length, Content-Range");
  h.set("X-Content-Type-Options", "nosniff");
  return h;
}
function error(status, code, message) {
  const h = common(new Headers({ "Content-Type": "application/json", "Cache-Control": "no-store" }));
  return new Response(`${JSON.stringify({ error: { code, message } })}
`, { status, headers: h });
}
var notFound = () => error(404, "not_found", "not found");
var denied = () => error(403, "denied", "denied");
var expired = () => error(401, "token_expired", "fetch a new token");
function ext(path) {
  const i = path.lastIndexOf(".");
  return i < 0 || path.lastIndexOf("/") > i ? "" : path.slice(i);
}
function createWorker(opts = {}) {
  const now = opts.now ?? (() => Math.floor(Date.now() / 1e3));
  const ttl = opts.ttlMs ?? 6e4;
  const missTtl = opts.missTtlMs ?? 5e3;
  const routes = /* @__PURE__ */ new Map();
  let keys;
  let secrets;
  async function route(env, pid) {
    const hit = routes.get(pid);
    if (hit && hit.until > Date.now()) return hit.value;
    const obj = await env.MEDIA.get(`_routes/${pid}.json`);
    const value = obj ? await obj.json() : null;
    routes.set(pid, { value, until: Date.now() + (value ? ttl : missTtl) });
    if (routes.size > 1e4) routes.delete(routes.keys().next().value);
    return value;
  }
  async function signingKeys(env) {
    if (keys && keys.until > Date.now()) return keys.value;
    const obj = await env.MEDIA.get("_keys.json");
    const value = obj ? await obj.json() : null;
    keys = { value, until: Date.now() + ttl };
    return value;
  }
  function secret(env, version) {
    if (!secrets || secrets.raw !== env.CONTENT_KEYS) {
      const ring = /* @__PURE__ */ new Map();
      for (const [v, hex] of Object.entries(JSON.parse(env.CONTENT_KEYS || "{}"))) {
        const bytes = new Uint8Array((hex.match(/../g) ?? []).map((b) => Number.parseInt(b, 16)));
        ring.set(
          Number(v),
          crypto.subtle.importKey("raw", bytes, { name: "HMAC", hash: "SHA-256" }, false, ["sign"])
        );
      }
      secrets = { raw: env.CONTENT_KEYS, ring };
    }
    return secrets.ring.get(version);
  }
  async function authorize(env, r, pid, raw) {
    const tok = parse(raw);
    if (!tok) return denied();
    const ks = await signingKeys(env);
    const key = ks?.keys.find((k) => k.kid === tok.kid);
    const t = now();
    if (!ks || !key || key.not_after !== null && key.not_after <= t) return denied();
    if (key.app_id !== null && key.app_id !== r.app_id) return denied();
    if (!await verify(tok, key.jwk)) return denied();
    const c = tok.claims;
    if (c.sub !== pid || c.aud !== "v" && c.aud !== "t" || c.iat <= 0 || c.iat > t + CLOCK_SKEW || c.exp - c.iat > ks.max_token_lifetime_seconds) {
      return denied();
    }
    if (c.exp <= t) return expired();
    return { aud: c.aud, vid: c.vid, internal: key.app_id === null };
  }
  async function contentKey(env, r, outputID, version) {
    const v = Number(version);
    const k = /^[0-9]+$/.test(version) ? secret(env, v) : void 0;
    if (!k || r.outputs[outputID] !== v) return notFound();
    const mac = await crypto.subtle.sign(
      "HMAC",
      await k,
      new TextEncoder().encode(`clipmux-content-key-v1
${r.asset_id}
${outputID}`)
    );
    const h = common(new Headers({ "Content-Type": "application/octet-stream", "Cache-Control": "no-store" }));
    h.set("Content-Length", "16");
    return new Response(new Uint8Array(mac).slice(0, 16), { headers: h });
  }
  async function master(env, r, req) {
    if (r.backend === "origin") {
      return origin(env, req, null);
    }
    const obj = await env.MEDIA.get(`media/${r.asset_id}/${r.output_id}/master.m3u8`);
    if (!obj) return error(500, "internal", "internal error");
    const text = await obj.text();
    const h = common(
      new Headers({ "Content-Type": CONTENT_TYPES[".m3u8"] ?? "", "Cache-Control": "no-store" })
    );
    const subs = Object.entries(r.tracks ?? {}).filter(([, t]) => t.listed).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([id, t]) => ({ id, name: t.name, language: t.language }));
    return text_(req, masterWithSubtitles(rewriteMaster(text, r.output_id), subs), h);
  }
  async function track(env, req, r, rel) {
    const m = TRACK_PATH.exec(rel);
    const t = m ? r.tracks?.[m[1]] : void 0;
    if (!m || !t) return notFound();
    const id = m[1];
    if (m[2] === "m3u8") {
      if (t.kind !== "subtitles") return notFound();
      const h2 = common(
        new Headers({ "Content-Type": CONTENT_TYPES[".m3u8"] ?? "", "Cache-Control": "no-store" })
      );
      return text_(req, subtitlePlaylist(id, t.duration_ms), h2);
    }
    const scope = r.policy === "signed" ? "private" : "public";
    if (r.backend === "origin") {
      return finish(await origin(env, req, null), "text/vtt; charset=utf-8", `${scope}, ${IMMUTABLE}`, req);
    }
    const key = `media/${r.asset_id}/tracks/${id}.vtt`;
    const edge = cacheFor();
    let sealed;
    const hit = edge ? await edge.match(new Request(cacheURL(key))) : void 0;
    if (hit) sealed = new Uint8Array(await hit.arrayBuffer());
    if (!sealed) {
      const obj = await env.MEDIA.get(key);
      if (!obj) return notFound();
      sealed = new Uint8Array(await new Response(obj.body).arrayBuffer());
      if (edge)
        await remember(
          key,
          new Response(sealed.slice()),
          new Headers({ "Content-Type": "application/octet-stream" })
        );
    }
    const plain = await openTrack(env, r.asset_id, id, key, sealed);
    if (!plain) return error(500, "internal", "internal error");
    const h = common(
      new Headers({ "Content-Type": "text/vtt; charset=utf-8", "Cache-Control": `${scope}, ${IMMUTABLE}` })
    );
    h.set("Content-Length", String(plain.length));
    return new Response(req.method === "HEAD" ? null : plain, { headers: h });
  }
  async function openTrack(env, assetID, trackID, key, sealed) {
    if (sealed.length < 17 + 16 || sealed[0] !== 1) return null;
    const version = new DataView(sealed.buffer, sealed.byteOffset, sealed.byteLength).getUint32(1);
    const s = secret(env, version);
    if (!s) return null;
    const enc = new TextEncoder();
    const mac = await crypto.subtle.sign(
      "HMAC",
      await s,
      enc.encode(`clipmux-track-key-v1
${assetID}
${trackID}`)
    );
    const aes = await crypto.subtle.importKey("raw", new Uint8Array(mac).slice(0, 16), "AES-GCM", false, [
      "decrypt"
    ]);
    try {
      const plain = await crypto.subtle.decrypt(
        { name: "AES-GCM", iv: sealed.slice(5, 17), additionalData: enc.encode(key) },
        aes,
        sealed.slice(17)
      );
      return new Uint8Array(plain);
    } catch {
      return null;
    }
  }
  async function origin(env, req, cacheKey) {
    if (!env.ORIGIN_URL) return error(500, "internal", "no origin");
    const u = new URL(req.url);
    const upstream = new Request(env.ORIGIN_URL.replace(/\/$/, "") + u.pathname, {
      method: req.method,
      headers: { ...rangeOnly(req), [EDGE_HEADER]: "1" }
    });
    const res = await (opts.fetchOrigin ?? fetch)(upstream);
    const h = common(new Headers(res.headers));
    if (cacheKey && res.status === 200) await remember(cacheKey, res.clone(), h);
    return new Response(res.body, { status: res.status, headers: h });
  }
  const cacheFor = () => opts.cache !== void 0 ? opts.cache : typeof caches !== "undefined" ? caches.default : null;
  const cacheURL = (key) => `https://edge-cache.clipmux.invalid/${key}`;
  async function remember(key, res, h) {
    const cache = cacheFor();
    if (!cache) return;
    const stored = new Headers(h);
    stored.set("Cache-Control", `public, ${IMMUTABLE}`);
    await cache.put(cacheURL(key), new Response(res.body, { status: 200, headers: stored }));
  }
  async function object2(env, req, r, outputID, rel, cache) {
    const ctype = CONTENT_TYPES[ext(rel)];
    if (!ctype || rel === "manifest.json" || !OUTPUT_PATH.test(rel)) return notFound();
    const key = `media/${r.asset_id}/${outputID}/${rel}`;
    const scope = r.policy === "signed" ? "private" : "public";
    const pinned = cache === IMMUTABLE;
    const edge = pinned ? cacheFor() : null;
    if (edge) {
      const hit = await edge.match(new Request(cacheURL(key), { headers: rangeOnly(req) }));
      if (hit) return finish(hit, ctype, `${scope}, ${cache}`, req);
    }
    if (r.backend === "origin") {
      const res = await origin(env, req, pinned ? key : null);
      return finish(res, ctype, `${scope}, ${cache}`, req);
    }
    const range = req.headers.get("Range");
    if (range) return ranged(env, req, key, range, ctype, `${scope}, ${cache}`);
    const obj = await env.MEDIA.get(key);
    if (!obj) return notFound();
    const h = common(new Headers({ "Content-Type": ctype, "Cache-Control": `${scope}, ${cache}` }));
    h.set("Content-Length", String(obj.size));
    h.set("Accept-Ranges", "bytes");
    if (edge && req.method === "GET") {
      const [a, b] = obj.body.tee();
      await remember(key, new Response(a), h);
      return new Response(b, { headers: h });
    }
    return new Response(req.method === "HEAD" ? null : obj.body, { headers: h });
  }
  async function ranged(env, req, key, header, ctype, cache) {
    const head = await env.MEDIA.head(key);
    if (!head) return notFound();
    const size = head.size;
    const h = common(new Headers({ "Content-Type": ctype, "Cache-Control": cache, "Accept-Ranges": "bytes" }));
    const r = resolveRange(header, size);
    if (r === "unsatisfiable") {
      h.set("Content-Range", `bytes */${size}`);
      return new Response(null, { status: 416, headers: h });
    }
    const obj = await env.MEDIA.get(key, r ? { range: { offset: r.start, length: r.end - r.start + 1 } } : {});
    if (!obj) return notFound();
    if (!r) {
      h.set("Content-Length", String(size));
      return new Response(req.method === "HEAD" ? null : obj.body, { headers: h });
    }
    h.set("Content-Range", `bytes ${r.start}-${r.end}/${size}`);
    h.set("Content-Length", String(r.end - r.start + 1));
    return new Response(req.method === "HEAD" ? null : obj.body, { status: 206, headers: h });
  }
  let viewerKeyring;
  async function viewerKey(env, appID, vid) {
    const raw = env.ANALYTICS_KEY ?? "";
    if (vid === "" || !/^[0-9a-f]{64}$/.test(raw)) return "";
    if (viewerKeyring?.raw !== raw) {
      const bytes = new Uint8Array((raw.match(/../g) ?? []).map((b) => Number.parseInt(b, 16)));
      viewerKeyring = {
        raw,
        key: crypto.subtle.importKey("raw", bytes, { name: "HMAC", hash: "SHA-256" }, false, ["sign"])
      };
    }
    const enc = new TextEncoder();
    const a = enc.encode(appID);
    const v = enc.encode(vid);
    const msg = new Uint8Array(a.length + 1 + v.length);
    msg.set(a);
    msg.set(v, a.length + 1);
    const mac = new Uint8Array(await crypto.subtle.sign("HMAC", await viewerKeyring.key, msg));
    return [...mac.slice(0, 16)].map((b) => b.toString(16).padStart(2, "0")).join("");
  }
  async function count(env, req, res, seen) {
    const r = seen.route;
    const dataset = env.ANALYTICS;
    if (!dataset || !r || req.method !== "GET" || res.status !== 200 && res.status !== 206) return res;
    if (seen.internal && !r.share) return res;
    const k = kindOf(seen.path ?? "");
    if (!k) return res;
    let views = 0;
    let watch = 0;
    if (res.status === 200 || (res.headers.get("Content-Range") ?? "").startsWith("bytes 0-")) {
      if (k.kind === "master") views = 1;
      else if (k.kind === "segment" && (k.rendition === "audio" || r.no_audio?.includes(k.outputID)))
        watch = SEGMENT_SECONDS;
    }
    const viewer = await viewerKey(env, r.app_id, seen.vid ?? "");
    const write = (bytes) => dataset.writeDataPoint({
      blobs: [env.PROJECT_ID ?? "", r.app_id, r.asset_id, seen.pid ?? "", k.kind, viewer],
      doubles: [views, watch, bytes],
      indexes: [r.asset_id]
    });
    const length = res.headers.get("Content-Length");
    if (length !== null && /^[0-9]+$/.test(length)) {
      write(Number(length));
      return res;
    }
    if (!res.body) {
      write(0);
      return res;
    }
    let n = 0;
    const counter = new TransformStream({
      transform(chunk, c) {
        n += chunk.byteLength;
        c.enqueue(chunk);
      },
      flush() {
        write(n);
      }
    });
    return new Response(res.body.pipeThrough(counter), { status: res.status, headers: res.headers });
  }
  async function serve(req, env, seen) {
    const url = new URL(req.url);
    if (req.method === "OPTIONS" && /^\/[vp]\//.test(url.pathname)) {
      const h = common(new Headers());
      h.set("Access-Control-Allow-Methods", "GET, HEAD");
      h.set("Access-Control-Allow-Headers", "Range");
      h.set("Access-Control-Max-Age", "86400");
      return new Response(null, { status: 204, headers: h });
    }
    if (req.method !== "GET" && req.method !== "HEAD") return notFound();
    const m = /^\/(?:v\/([^/]+)|(p))\/([^/]+)\/(.+)$/.exec(url.pathname);
    if (!m) return notFound();
    const [, token, , pid, path] = m;
    const r = await route(env, pid);
    if (!r) return notFound();
    if (r.denied) return denied();
    let audience = "v";
    if (r.policy === "signed") {
      if (token === void 0) return denied();
      const a = await authorize(env, r, pid, token);
      if (a instanceof Response) return a;
      audience = a.aud;
      seen.internal = a.internal;
      seen.vid = a.vid;
    }
    Object.assign(seen, { route: r, pid, path });
    if (path === "master.m3u8" && audience === "v") return master(env, r, req);
    if (path === "poster.jpg") return object2(env, req, r, r.output_id, "poster.jpg", "max-age=300");
    if (path.startsWith("tracks/")) return audience === "v" ? track(env, req, r, path.slice(7)) : denied();
    if (audience !== "v" && !path.endsWith("/poster.jpg")) return denied();
    const slash = path.indexOf("/");
    if (slash < 0) return notFound();
    const outputID = path.slice(0, slash);
    const rest = path.slice(slash + 1);
    if (!(outputID in r.outputs)) return notFound();
    if (rest.startsWith("key/")) return contentKey(env, r, outputID, rest.slice(4));
    if (audience !== "v" && rest !== "poster.jpg") return denied();
    return object2(env, req, r, outputID, rest, IMMUTABLE);
  }
  return {
    async fetch(req, env) {
      try {
        const seen = {};
        return await count(env, req, await serve(req, env, seen), seen);
      } catch (err) {
        console.error("delivery", err instanceof Error ? err.message : String(err));
        return error(500, "internal", "internal error");
      }
    }
  };
}
function rewriteMaster(master, outputID) {
  let out = "";
  for (const raw of master.split(/(?<=\n)/)) {
    let line = raw.replace(/[\r\n]+$/, "");
    if (line.startsWith("#")) line = line.replaceAll('URI="', `URI="${outputID}/`);
    else if (line !== "") line = `${outputID}/${line}`;
    out += `${line}
`;
  }
  return out;
}
function masterWithSubtitles(master, subs) {
  if (subs.length === 0) return master;
  const attr = (s) => s.replace(/["\r\n]/g, "");
  const media = subs.map(
    (s) => `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="${attr(s.name)}",LANGUAGE="${attr(s.language)}",AUTOSELECT=YES,DEFAULT=NO,URI="tracks/${s.id}.m3u8"
`
  ).join("");
  let out = "";
  let inserted = false;
  for (const line of master.split(/(?<=\n)/)) {
    if (line.startsWith("#EXT-X-STREAM-INF:")) {
      if (!inserted) {
        out += media;
        inserted = true;
      }
      out += `${line.replace(/\n$/, "")},SUBTITLES="subs"
`;
      continue;
    }
    out += line;
  }
  return inserted ? out : out + media;
}
function subtitlePlaylist(trackID, durationMS) {
  const target = Math.max(Math.ceil(durationMS / 1e3), 1);
  const extinf = `${Math.floor(durationMS / 1e3)}.${String(durationMS % 1e3).padStart(3, "0")}`;
  return `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:${target}
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:${extinf},
${trackID}.vtt
#EXT-X-ENDLIST
`;
}
function text_(req, body, h) {
  const bytes = new TextEncoder().encode(body);
  h.set("Content-Length", String(bytes.length));
  return new Response(req.method === "HEAD" ? null : bytes, { headers: h });
}
function kindOf(p) {
  if (p === "master.m3u8") return { kind: "master", rendition: "", outputID: "" };
  if (p === "poster.jpg" || p.endsWith("/poster.jpg")) return { kind: "poster", rendition: "", outputID: "" };
  if (p.startsWith("tracks/"))
    return { kind: p.endsWith(".m3u8") ? "track_playlist" : "track", rendition: "", outputID: "" };
  const parts = p.split("/");
  if (parts.length !== 3) return null;
  const [outputID = "", rendition = "", file = ""] = parts;
  let kind = "segment";
  if (rendition === "key") kind = "key";
  else if (file === "index.m3u8") kind = "playlist";
  else if (file === "init.mp4") kind = "init";
  return { kind, rendition, outputID };
}
function rangeOnly(req) {
  const r = req.headers.get("Range");
  return r ? { Range: r } : {};
}
function finish(res, ctype, cache, req) {
  const h = common(new Headers(res.headers));
  h.set("Content-Type", ctype);
  h.set("Cache-Control", cache);
  return new Response(req.method === "HEAD" ? null : res.body, { status: res.status, headers: h });
}
function resolveRange(header, size) {
  const m = /^bytes=(\d*)-(\d*)$/.exec(header.trim());
  if (!m) return null;
  const [, a = "", b = ""] = m;
  if (a === "" && b === "") return null;
  if (a === "") {
    const n = Number(b);
    if (n === 0) return "unsatisfiable";
    return { start: Math.max(0, size - n), end: size - 1 };
  }
  const start = Number(a);
  if (start >= size) return "unsatisfiable";
  const end = b === "" ? size - 1 : Math.min(Number(b), size - 1);
  if (end < start) return null;
  return { start, end };
}

// src/index.ts
var index_default = createWorker();
export {
  index_default as default
};
