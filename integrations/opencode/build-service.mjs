import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const directory = fileURLToPath(new URL('.', import.meta.url))
const manifest = JSON.parse(readFileSync(new URL('node_modules/@opencode/client/package.json', import.meta.url), 'utf8'))
if (manifest.version !== '2.3.2') throw new Error('Revalidate service discovery before changing the OpenCode client version')
const license = readFileSync(new URL('service.LICENSE', import.meta.url), 'utf8')

execFileSync('npm', [
  'exec', '--yes', '--package=esbuild@0.25.12', '--', 'esbuild',
  '--bundle', '--platform=node', '--format=esm', '--target=node22', '--minify',
  '--legal-comments=inline', '--outfile=service.js', `--banner:js=/*\n${license}*/`,
], {
  cwd: directory,
  input: 'export { discover, headers } from "@opencode/client/service"\n',
  stdio: ['pipe', 'inherit', 'inherit'],
})
