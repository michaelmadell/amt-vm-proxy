// End-to-end KVM test: runs the REAL ui-toolkit KVM client (TypeScript, from a
// ui-toolkit checkout) in Node against relay -> fake-amt -> fakevnc.
//
//   cd <ui-toolkit checkout> && npm ci && npm i --no-save tsx
//   UI_TOOLKIT=$PWD BPP=2 FAKEVNC_LOG=/tmp/fakevnc.jsonl npx tsx /path/to/fake-amt/test/kvm-client.ts
//
// Exits non-zero if a protocol/pixel check fails. Pointer-geometry results are
// printed as a report: they describe ui-toolkit's mouse mapping, not fake-amt.
import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'

const UI = process.env.UI_TOOLKIT ?? '/tmp/ui-toolkit'
const BPP = Number(process.env.BPP ?? '2')
const RELAY = process.env.RELAY ?? 'ws://127.0.0.1:8181/relay'
const VNCLOG = process.env.FAKEVNC_LOG ?? '/tmp/fakevnc.jsonl'
const IDLE_SECONDS = Number(process.env.IDLE_SECONDS ?? '0')

const failures: string[] = []
const ok = (cond: boolean, msg: string): void => {
  console.log(`${cond ? 'PASS' : 'FAIL'}  ${msg}`)
  if (!cond) failures.push(msg)
}
const sleep = async (ms: number): Promise<void> => await new Promise((r) => setTimeout(r, ms))
async function waitFor(what: string, fn: () => boolean, ms = 15000): Promise<boolean> {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    if (fn()) return true
    await sleep(25)
  }
  console.log(`      (timed out waiting for ${what})`)
  return false
}

// ---- minimal DOM: FileReader, canvas that records what's painted ----------
class NodeFileReader {
  result: string | null = null
  onload: ((e: any) => void) | null = null
  onloadend: ((e: any) => void) | null = null
  readAsBinaryString(blob: Blob): void {
    void blob.arrayBuffer().then((buf) => {
      this.result = Buffer.from(buf).toString('latin1')
      this.onload?.({ target: this })
      this.onloadend?.({ target: this })
    })
  }
}
;(globalThis as any).document = {
  hidden: false,
  addEventListener() {},
  removeEventListener() {},
  createElement: () => makeCanvas().canvas
}

interface Geometry { offsetWidth: number, offsetHeight: number, offsetLeft: number, offsetTop: number, bcrLeft: number, bcrTop: number }
function makeCanvas() {
  let w = 0
  let h = 0
  let fb = new Uint8ClampedArray(0)
  let painted = new Uint8Array(0)
  let paintedCount = 0
  const geo: Geometry = { offsetWidth: 0, offsetHeight: 0, offsetLeft: 0, offsetTop: 0, bcrLeft: 0, bcrTop: 0 }
  const realloc = (): void => { fb = new Uint8ClampedArray(w * h * 4); painted = new Uint8Array(w * h); paintedCount = 0 }
  const canvas: any = {
    get width () { return w }, set width (v: number) { if (v !== w) { w = v; realloc() } },
    get height () { return h }, set height (v: number) { if (v !== h) { h = v; realloc() } },
    get offsetWidth () { return geo.offsetWidth || w }, get offsetHeight () { return geo.offsetHeight || h },
    get offsetLeft () { return geo.offsetLeft }, get offsetTop () { return geo.offsetTop },
    offsetParent: null,
    parentElement: null,
    style: {},
    getBoundingClientRect: () => ({ left: geo.bcrLeft, top: geo.bcrTop, width: canvas.offsetWidth, height: canvas.offsetHeight }),
    addEventListener() {}, removeEventListener() {}
  }
  const ctx: any = {
    canvas,
    fillStyle: '',
    fillRect() {}, drawImage() {}, setTransform() {}, rotate() {},
    createImageData: (iw: number, ih: number) => ({ width: iw, height: ih, data: new Uint8ClampedArray(iw * ih * 4) }),
    putImageData (img: any, x: number, y: number) {
      for (let yy = 0; yy < img.height; yy++) {
        for (let xx = 0; xx < img.width; xx++) {
          const X = x + xx; const Y = y + yy
          if (X >= w || Y >= h) continue
          const s = (yy * img.width + xx) * 4; const d = (Y * w + X) * 4
          fb[d] = img.data[s]; fb[d + 1] = img.data[s + 1]; fb[d + 2] = img.data[s + 2]; fb[d + 3] = img.data[s + 3]
          if (painted[Y * w + X] === 0) { painted[Y * w + X] = 1; paintedCount++ }
        }
      }
    }
  }
  return { canvas, ctx, geo, pixel: (x: number, y: number) => [fb[(y * w + x) * 4], fb[(y * w + x) * 4 + 1], fb[(y * w + x) * 4 + 2]], fullyPainted: () => w > 0 && paintedCount === w * h }
}

// Same pattern as test/fakevnc, then quantised exactly as bridge + ui-toolkit do.
function pattern (x: number, y: number, gen: number): number[] { return [(x * 3 + gen * 40) & 255, (y * 5) & 255, (x ^ y) & 255] }
function expectedDisplayed (x: number, y: number, gen: number): number[] {
  const [r, g, b] = pattern(x, y, gen)
  const q = (c: number, max: number): number => Math.floor((c * max + 127) / 255)
  if (BPP === 1) {
    const v = (q(r, 7) << 5) | (q(g, 7) << 2) | q(b, 3)
    const fix = (c: number): number => (c > 127 ? c + 32 : c)
    return [v & 224, (v & 28) << 3, fix((v & 3) << 6)]
  }
  const v = (q(r, 31) << 11) | (q(g, 63) << 5) | q(b, 31)
  return [(v >> 8) & 248, (v >> 3) & 252, (v & 31) << 3]
}

function vncEvents (): any[] {
  try { return readFileSync(VNCLOG, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l)) } catch { return [] }
}

async function main (): Promise<void> {
  const ui = await import(pathToFileURL(`${UI}/src/core/index.ts`).href)
  const { AMTDesktop, AMTKvmDataRedirector, DataProcessor, MouseHelper, Protocol, CommsHelper } = ui
  const cv = makeCanvas()

  // Wired exactly like ui-toolkit-angular's kvm.component.ts instantiate().
  const redirector = new AMTKvmDataRedirector({
    mode: 'kvm', protocol: Protocol.KVM, fr: new NodeFileReader(), host: 'test-guid', port: 16994,
    user: '', pass: '', tls: 0, tls1only: 0, authToken: 'test-token', server: RELAY
  })
  const module = new AMTDesktop(cv.ctx)
  const dataProcessor = new DataProcessor(redirector, module)
  const mouse = new MouseHelper(module, redirector, 200)
  let lastState = -1
  let errored = false
  redirector.onProcessData = module.processData.bind(module)
  redirector.onStart = module.start.bind(module)
  redirector.onNewState = module.onStateChange.bind(module)
  redirector.onSendKvmData = module.onSendKvmData.bind(module)
  redirector.onStateChanged = (_r: any, s: number) => { lastState = s }
  redirector.onError = () => { errored = true }
  module.onSend = redirector.send.bind(redirector)
  module.onProcessData = dataProcessor.processData.bind(dataProcessor)
  module.bpp = BPP
  const quiet = console.debug; console.debug = () => {}; const log = console.log
  console.log = (...a: any[]) => { if (typeof a[0] === 'string' && /^(PASS|FAIL|  |---|==)/.test(a[0])) log(...a) }

  console.log(`=== ui-toolkit KVM client, bpp=${BPP} (${BPP === 1 ? 'RGB332' : 'RGB565'}), relay ${RELAY}`)
  redirector.start(WebSocket as any)

  ok(await waitFor('first frame', () => cv.fullyPainted() && cv.canvas.width === 1280), 'handshake via Console relay + AMT digest auth, RFB up, first 1280x800 frame fully painted')
  const checkPattern = (gen: number, label: string): void => {
    let bad = 0; let first = ''
    for (let y = 0; y < cv.canvas.height; y += 7) {
      for (let x = 0; x < cv.canvas.width; x += 5) {
        const got = cv.pixel(x, y); const want = expectedDisplayed(x, y, gen)
        if (got[0] !== want[0] || got[1] !== want[1] || got[2] !== want[2]) { bad++; if (first === '') first = `(${x},${y}) got ${got} want ${want}` }
      }
    }
    ok(bad === 0, `${label}: every sampled pixel is in the right place with the right colour${bad > 0 ? ` (${bad} wrong, first ${first})` : ''}`)
  }
  checkPattern(0, 'frame 1280x800 (one big RAW rect re-tiled to 64x64)')

  ok(await waitFor('resize', () => cv.canvas.width === 1024 && cv.canvas.height === 768 && cv.fullyPainted()), 'desktop resize 1280x800 -> 1024x768 delivered (DesktopSize) and repainted')
  checkPattern(1, 'frame after resize')

  const enc = vncEvents().filter((e) => e.ev === 'encodings').pop()
  ok(enc !== undefined && enc.list.join(',') === '0,-223', `bridge asked VNC server only for RAW + DesktopSize (got ${enc?.list})`)
  const pfe = vncEvents().filter((e) => e.ev === 'pixelformat').pop()
  ok(pfe?.bpp === 32, 'bridge pinned the VNC server to 32bpp and converts for the browser')

  // ---- keyboard ----
  CommsHelper.sendKey(redirector, 0x61, 1); CommsHelper.sendKey(redirector, 0x61, 0)
  ok(await waitFor('key', () => vncEvents().some((e) => e.ev === 'key' && e.keysym === 0x61 && e.down === false), 3000), 'key events reach the VM (keysym 0x61 down/up)')

  // ---- pointer geometry report ----
  console.log('--- pointer mapping (ui-toolkit MouseHelper.mousemove -> what the VM receives), framebuffer 1024x768')
  const fbw = 1024; const fbh = 768
  const cases: Array<{ name: string, geo: Partial<Geometry>, aim: [number, number], page: (g: Geometry) => [number, number], note?: string }> = []
  const base: Geometry = { offsetWidth: fbw, offsetHeight: fbh, offsetLeft: 0, offsetTop: 0, bcrLeft: 0, bcrTop: 0 }
  // Canvas drawn at a uniform scale s, at page offset (L,T): page = L + aim*s
  const uniform = (s: number, L = 0, T = 0) => (g: Geometry): [number, number] => [L + 0, T + 0]
  void uniform
  cases.push({ name: '1:1, canvas at page origin', geo: {}, aim: [700, 500], page: () => [700, 500] })
  cases.push({ name: '1:1, canvas offset 120,80 in page', geo: { offsetLeft: 120, offsetTop: 80, bcrLeft: 120, bcrTop: 80 }, aim: [700, 500], page: () => [820, 580] })
  cases.push({ name: 'uniform scale 0.6 (aspect kept)', geo: { offsetWidth: fbw * 0.6, offsetHeight: fbh * 0.6 }, aim: [700, 500], page: () => [700 * 0.6, 500 * 0.6] })
  cases.push({
    name: 'non-uniform: element 1000x768 (width squeezed)', geo: { offsetWidth: 1000, offsetHeight: 768 }, aim: [700, 500], page: () => [700 * 1000 / fbw, 500],
    note: 'happens when max-width clamps width but height is forced, e.g. .fullscreen{height:100vh!important}'
  })
  {
    // Fullscreen: browser UA style makes the element fill the screen with object-fit:contain.
    const sw = 1920; const sh = 1080; const s = Math.min(sw / fbw, sh / fbh); const dw = fbw * s; const ox = (sw - dw) / 2
    cases.push({
      name: `fullscreen 1920x1080, letterboxed (content ${dw.toFixed(0)}px wide, bars ${ox.toFixed(0)}px)`, geo: { offsetWidth: sw, offsetHeight: sh },
      aim: [700, 500], page: () => [ox + 700 * s, 500 * s], note: 'UA :fullscreen style = width/height 100% + object-fit:contain'
    })
  }
  cases.push({
    name: 'after page scrolled 200px (canvas at top 80)', geo: { offsetLeft: 0, offsetTop: 80, bcrLeft: 0, bcrTop: 80 - 200 }, aim: [700, 500], page: () => [700, 580],
    note: 'MouseHelper caches the largest bcr.top seen and adds the difference'
  })

  const results: string[] = []
  for (const c of cases) {
    Object.assign(cv.geo, base, c.geo)
    mouse.resetOffsets()
    if (c.name.startsWith('after page scrolled')) {
      // first a move while unscrolled, as a user would have done
      Object.assign(cv.geo, { bcrTop: 80 })
      mouse.mousemove({ pageX: 10, pageY: 90, preventDefault () {}, stopPropagation () {} } as any)
      Object.assign(cv.geo, c.geo)
    }
    const before = vncEvents().length
    const [px, py] = c.page(cv.geo)
    mouse.mousemove({ pageX: px, pageY: py, preventDefault () {}, stopPropagation () {} } as any)
    await waitFor('pointer', () => vncEvents().slice(before).some((e) => e.ev === 'pointer'), 3000)
    const ev = vncEvents().slice(before).filter((e) => e.ev === 'pointer').pop()
    const dx = ev ? ev.x - c.aim[0] : NaN; const dy = ev ? ev.y - c.aim[1] : NaN
    const good = Math.abs(dx) <= 1 && Math.abs(dy) <= 1
    results.push(`${good ? '  ok ' : '  OFF'}  ${c.name}: aimed at (${c.aim}), VM got (${ev?.x},${ev?.y})  error (${dx >= 0 ? '+' : ''}${dx},${dy >= 0 ? '+' : ''}${dy})${c.note && !good ? `\n          ${c.note}` : ''}`)
  }
  results.forEach((r) => console.log(r))
  ok(results.slice(0, 3).every((r) => r.startsWith('  ok')), 'pointer events pass through the bridge unchanged (simple layouts land exactly)')

  if (IDLE_SECONDS > 0) {
    console.log(`--- idling ${IDLE_SECONDS}s with a static desktop (Console drops relays with no device data for 30s)`)
    await sleep(IDLE_SECONDS * 1000)
    const before = vncEvents().length
    mouse.mousemove({ pageX: 5, pageY: 5, preventDefault () {}, stopPropagation () {} } as any)
    const alive = await waitFor('pointer after idle', () => vncEvents().slice(before).some((e) => e.ev === 'pointer'), 3000)
    console.log(`  session after idle: ${alive ? 'ALIVE' : 'DROPPED'}`)
    if (process.env.EXPECT_IDLE === 'dropped') ok(!alive, 'without keepalive the relay drops the idle session (control case)')
    else ok(alive, 'session survives an idle static desktop')
  }

  ok(!errored, 'no client-side errors')
  void lastState
  console.debug = quiet
  redirector.stop?.()
  console.log(failures.length === 0 ? '== ALL CHECKS PASSED' : `== ${failures.length} CHECK(S) FAILED`)
  process.exit(failures.length === 0 ? 0 : 1)
}

main().catch((e) => { console.error(e); process.exit(2) })
