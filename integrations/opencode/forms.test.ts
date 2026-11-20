import assert from 'node:assert/strict'
import { test } from 'node:test'
import { formAnswer, formQuestions } from './forms.ts'
import type { FormInfo } from '@opencode/client'

test('keyed form answers retain scalar and multi-select types and skip optional blanks', () => {
  const form: FormInfo = { id: 'form', sessionID: 'ses', title: 'Settings', fields: [
    { type: 'integer', key: 'count', required: true }, { type: 'number', key: 'ratio' }, { type: 'string', key: 'name' },
    { type: 'multiselect', key: 'tags', options: [{ label: 'First', value: 'first' }] },
  ] }
  assert.deepEqual(formAnswer(form, [['2'], ['0.5'], [], ['First']]), { count: 2, ratio: 0.5, tags: ['first'] })
  assert.throws(() => formAnswer(form, [['2.5'], [], [], []]), /numeric/)
  assert.throws(() => formAnswer(form, [['2'], [], [], ['unknown']]), /Unknown/)
  assert.throws(() => formAnswer(form, [['2']]), /Missing/)
})

test('conditional, external, hidden and ambiguously-labelled forms are terminal-only', () => {
  for (const field of [
    { type: 'string', key: 'value', hidden: true },
    { type: 'string', key: 'value', when: [{ key: 'other', op: 'eq', value: 'yes' }] },
    { type: 'external', key: 'value', url: 'https://example.com' },
    { type: 'string', key: 'value', options: [{ label: 'Same', value: 'a' }, { label: 'Same', value: 'b' }] },
  ]) assert.equal(formQuestions({ id: 'form', sessionID: 'ses', title: 'Question', fields: [field] } as FormInfo).canAnswer, false)
})
