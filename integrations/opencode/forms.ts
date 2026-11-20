import type { FormInfo, FormAnswer } from '@opencode/client'

/** Only fields representable by Slorp's ordered question contract are remotely answerable. */
export function formQuestions(form: FormInfo) {
  const canAnswer = new Set(form.fields.map(f => f.key)).size === form.fields.length &&
    form.fields.every(f => f.type !== 'external' && !f.hidden && !f.when?.length &&
      (!('options' in f) || !f.options || new Set(f.options.map(o => o.label)).size === f.options.length))
  const questions = form.fields.map(f => ({
    question: f.description || f.title || f.key,
    header: f.title || form.title,
    options: f.type === 'boolean' ? [{ label: 'Yes', description: '' }, { label: 'No', description: '' }] :
      'options' in f ? (f.options ?? []).map(o => ({ label: o.label, description: o.description ?? '' })) : [],
    multiSelect: f.type === 'multiselect',
    custom: 'options' in f && f.options?.length ? f.custom === true : f.type !== 'boolean',
  }))
  return { questions, canAnswer }
}

export function formAnswer(form: FormInfo, selections: unknown): FormAnswer {
  if (!Array.isArray(selections) || selections.length !== form.fields.length) throw new Error('Missing form answers')
  const answer: FormAnswer = {}
  for (const [i, field] of form.fields.entries()) {
    const selected: unknown = selections[i]
    if (!Array.isArray(selected) || selected.some(v => typeof v !== 'string')) throw new Error('Invalid form answer')
    if (field.type === 'external') throw new Error('Unsupported form field')
    if (!selected.length && !field.required) continue
    const values = selected.map(label => {
      const option = 'options' in field ? field.options?.find(o => o.label === label) : undefined
      if (option) return option.value
      if ('options' in field && field.options?.length && field.custom !== true) throw new Error('Unknown form option')
      return label as string
    })
    if (field.type === 'multiselect') { answer[field.key] = values; continue }
    if (values.length !== 1) throw new Error('Expected one form answer')
    const value = values[0]!
    if (field.type === 'string') answer[field.key] = value
    else if (field.type === 'boolean') {
      if (value !== 'Yes' && value !== 'No') throw new Error('Invalid boolean answer')
      answer[field.key] = value === 'Yes'
    } else if (field.type === 'number' || field.type === 'integer') {
      const number = Number(value)
      if (!value.trim() || !Number.isFinite(number) || field.type === 'integer' && !Number.isInteger(number)) throw new Error('Invalid numeric answer')
      answer[field.key] = number
    } else throw new Error('Unsupported form field')
  }
  return answer
}
