import { fireEvent, render, screen, waitFor } from '@testing-library/vue'
import { afterEach, describe, expect, it } from 'vitest'
import ConfirmDialog from '../src/components/ConfirmDialog.vue'

describe('confirmation dialog', () => {
  let background: HTMLButtonElement | undefined
  afterEach(() => background?.remove())

  it('starts on Cancel, wraps focus, makes the background inert, and emits Escape cancellation', async () => {
    background = document.createElement('button')
    background.textContent = 'Background action'
    background.inert = false
    document.body.append(background)
    const { emitted, unmount } = render(ConfirmDialog, { props: { title: 'Remove Travel key?', description: 'This passkey cannot be used again.', confirmLabel: 'Remove passkey' } })
    const cancel = await screen.findByRole('button', { name: 'Cancel' })
    const confirm = screen.getByRole('button', { name: 'Remove passkey' })
    await waitFor(() => expect(cancel).toHaveFocus())
    expect(background.inert).toBe(true)
    await fireEvent.keyDown(cancel, { key: 'Tab', shiftKey: true })
    expect(confirm).toHaveFocus()
    await fireEvent.keyDown(confirm, { key: 'Tab' })
    expect(cancel).toHaveFocus()
    await fireEvent.keyDown(cancel, { key: 'Escape' })
    expect(emitted().cancel).toHaveLength(1)
    unmount()
    expect(background.inert).toBe(false)
  })
})
