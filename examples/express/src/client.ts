// The browser half: two custom elements, configured by their attributes.
// They talk only to this app's /api/clipmux/* routes.
import '@clipmux/player/element'
import '@clipmux/uploader/element'

const uploader = document.querySelector('clipmux-uploader')
uploader?.addEventListener('clipmux-uploaded', () => {
  // The webhook marks the video ready once it's encoded; reload until then.
  setInterval(() => location.reload(), 3000)
})

const player = document.querySelector('clipmux-player')
player?.addEventListener('clipmux-denied', () => {
  player.replaceWith(
    Object.assign(document.createElement('p'), { textContent: 'You may not watch this video.' }),
  )
})
