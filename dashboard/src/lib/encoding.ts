import type { Routing } from '@/api'

/** The cloud GPUs videos can encode on (D116). */
export const cloudNames: Record<string, string> = { modal: 'Modal', runpod: 'RunPod' }

/**
 * Where new videos encode, by name: a cloud GPU, or "this server" (which
 * the Encoding page also shows for the PC routings). Subtitles in the
 * local speech mode go to the same place (D127).
 */
export function encodedOn(r: Routing): string {
  return cloudNames[r.primary_executor] ?? 'this server'
}
