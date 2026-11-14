import assert from 'node:assert/strict'
import { test } from 'node:test'
import { EventWindow } from './events.ts'

const ev = (id: string, created_at: string) => ({ id, created_at, body: {} as never })

test('the window keeps the newest created_at as after, with its microseconds', () => {
  const w = new EventWindow(new Date('2027-01-01T00:00:00Z'))
  assert.equal(w.after, undefined)
  w.advance([ev('evt_1', '2027-01-01T00:01:00.000001Z'), ev('evt_2', '2027-01-01T00:01:00.000002Z')])
  assert.equal(w.after, '2027-01-01T00:01:00.000002Z')
  w.advance([])
  assert.equal(w.after, '2027-01-01T00:01:00.000002Z', 'an empty page changes nothing')
})

test('since follows the last event 30 s behind, and never moves back', () => {
  const w = new EventWindow(new Date('2027-01-01T00:00:00Z'))
  w.advance([ev('evt_1', '2027-01-01T00:00:10Z')])
  assert.equal(w.since, '2027-01-01T00:00:00.000Z', '10 s in is still inside the overlap')
  w.advance([ev('evt_2', '2027-01-01T00:05:00Z')])
  assert.equal(w.since, '2027-01-01T00:04:30.000Z')
})

test('a full page moves since to its last event exactly', () => {
  const w = new EventWindow(new Date('2027-01-01T00:00:00Z'))
  const page = Array.from({ length: 100 }, (_, i) =>
    ev(`evt_${i}`, `2027-01-01T00:00:${String(Math.floor(i / 2)).padStart(2, '0')}.5Z`),
  )
  w.advance(page)
  assert.equal(w.since, '2027-01-01T00:00:49.5Z')
})
