// End-to-end IDER test: runs the REAL ui-toolkit IDER client (AMTRedirector +
// AMTIDER) in Node against relay -> fake-amt, mounting a generated ISO image.
//
//   UI_TOOLKIT=<ui-toolkit checkout> RELAY=ws://127.0.0.1:8181/relay FAKEAMT_LOG=/tmp/amt.log \
//     npx tsx /path/to/fake-amt/test/ider-client.ts
//
// PULL=1 when fake-amt runs with -hyperv-vm: then the whole image must be read
// and fake-amt's logged sha256 must match. Exits non-zero on failure.
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'

const UI = process.env.UI_TOOLKIT ?? '/tmp/ui-toolkit'
const RELAY = process.env.RELAY ?? 'ws://127.0.0.1:8181/relay'
const AMTLOG = process.env.FAKEAMT_LOG ?? ''
const PULL = process.env.PULL === '1'
const SECTORS = Number(process.env.SECTORS ?? '1500') // ~3 MiB

const failures: string[] = []
const ok = (cond: boolean, msg: string): void => {
  console.log(`${cond ? 'PASS' : 'FAIL'}  ${msg}`)
  if (!cond) failures.push(msg)
}
const sleep = async (ms: number): Promise<void> => await new Promise((r) => setTimeout(r, ms))
async function waitFor (what: string, fn: () => boolean, ms = 30000): Promise<boolean> {
  const t0 = Date.now()
  while (Date.now() - t0 < ms) {
    if (fn()) return true
    await sleep(50)
  }
  console.log(`      (timed out waiting for ${what})`)
  return false
}
const amtLog = (): string => { try { return readFileSync(AMTLOG, 'utf8') } catch { return '' } }

class NodeFileReader {
  result: string | null = null
  onload: ((e: any) => void) | null = null
  readAsBinaryString (blob: Blob): void {
    void blob.arrayBuffer().then((buf) => {
      this.result = Buffer.from(buf).toString('latin1')
      this.onload?.({ target: this })
    })
  }
}
;(globalThis as any).FileReader = NodeFileReader

// An ISO-9660-looking image: a primary volume descriptor at sector 16 and a
// position-dependent pattern everywhere else.
function makeImage (): Buffer {
  const img = Buffer.alloc(SECTORS * 2048)
  for (let i = 0; i < img.length; i += 4) img.writeUInt32LE((i * 2654435761) >>> 0, i)
  const pvd = img.subarray(16 * 2048, 17 * 2048)
  pvd.fill(0x20, 0, 2048)
  pvd[0] = 1; pvd.write('CD001', 1, 'latin1'); pvd[6] = 1
  pvd.write('FAKEAMT', 8, 'latin1')
  pvd.write('FAKEAMT_TEST', 40, 'latin1')
  return img
}

async function main (): Promise<void> {
  const ui = await import(pathToFileURL(`${UI}/src/core/index.ts`).href)
  const { AMTRedirector, AMTIDER, Protocol } = ui
  const img = makeImage()
  const file = new File([img], 'test.iso')

  // Wired like ui-toolkit-react's IDER component (startIder()).
  const redirector = new AMTRedirector({
    mode: 'ider', protocol: Protocol.IDER, fr: new NodeFileReader(), host: 'test-guid', port: 16994,
    user: '', pass: '', tls: 0, tls1only: 0, authToken: 'test-token', server: RELAY
  })
  const ider = new AMTIDER(redirector, file, null)
  const reads = new Set<number>()
  let state = -1
  redirector.onNewState = ider.stateChange.bind(ider)
  redirector.onProcessData = ider.processData.bind(ider)
  redirector.onStateChanged = (_r: any, s: number) => { state = s }
  ider.sectorStats = (mode: number, _dev: number, _total: number, start: number, len: number) => {
    if (mode === 1) for (let i = start; i < start + len; i++) reads.add(i)
  }
  const log = console.log
  console.debug = () => {}
  console.log = (...a: any[]) => { if (typeof a[0] === 'string' && /^(PASS|FAIL|  |===)/.test(a[0])) log(...a) }

  console.log(`=== ui-toolkit IDER client, ${SECTORS}-sector image, relay ${RELAY}${PULL ? ', full copy expected' : ''}`)
  redirector.start(WebSocket as any)

  ok(await waitFor('IDER session', () => state === 3), 'IDER session up via relay + digest auth')
  ok(await waitFor('IDER enabled', () => ider.enabled === true), 'register toggle answered, drive reported enabled')
  ok(await waitFor('PVD read', () => reads.has(16)), 'host read the volume descriptor (sector 16)')
  ok(await waitFor('volume log', () => amtLog().includes('ISO 9660 volume "FAKEAMT_TEST"'), 10000), 'fake-amt decoded the volume label from the streamed data')

  if (PULL) {
    ok(await waitFor('full copy', () => amtLog().includes('copy finished'), 120000), `whole image copied (${reads.size}/${SECTORS} sectors read)`)
    const want = createHash('sha256').update(img).digest('hex')
    ok(amtLog().includes(`sha256 ${want}`), 'copied image sha256 matches the file the browser mounted')
  }

  await sleep(1500)
  ok(state === 3, 'session still up after the probe (no protocol error)')
  ider.stop() // = the UI's unmount
  ok(await waitFor('session end', () => amtLog().includes('IDER session ended'), 10000), 'unmount ends the session on fake-amt')
  if (PULL) {
    ok(await waitFor('eject', () => /Hyper-V: (ejected|inserting)/.test(amtLog()), 10000), 'fake-amt reached the Hyper-V insert/eject step')
  }

  log(failures.length === 0 ? 'ALL PASS' : `${failures.length} FAILED`)
  process.exit(failures.length === 0 ? 0 : 1)
}

void main()
