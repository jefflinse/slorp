import type { TuiPlugin } from '@opencode-ai/plugin/tui'
import { BroadcastChannel } from 'node:worker_threads'

/** Announces the selected conversation to the server bridge in this OpenCode process. */
const tui: TuiPlugin = async api => {
  const channel = new BroadcastChannel('slorp.opencode.presence')
  const announce = () => {
    const route = api.route.current
    channel.postMessage({
      directory: api.state.path.directory,
      ...(route.name === 'session' && typeof route.params?.sessionID === 'string' ? { sessionId: route.params.sessionID } : {}),
    })
  }
  announce()
  const timer = setInterval(announce, 2000)
  timer.unref?.()
  channel.unref()
  api.lifecycle.onDispose(() => { clearInterval(timer); channel.close() })
}

export default { id: 'slorp', tui }
