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

  it('keeps a busy operation open for repeated Escape and native cancel attempts', async () => {
    const { emitted, rerender } = render(ConfirmDialog, { props: { title: 'Remove Travel key?', description: 'This passkey cannot be used again.', confirmLabel: 'Remove passkey', busy: false, busyLabel: 'Removing passkey…' } })
    await rerender({ busy: true, error: '' })
    const dialog = screen.getByRole('alertdialog')
    const status = await screen.findByRole('status')
    await waitFor(() => expect(status).toHaveFocus())
    expect(status).toHaveTextContent('Removing passkey…')
    await fireEvent.keyDown(status, { key: 'Escape' })
    await fireEvent.keyDown(status, { key: 'Escape' })
    await fireEvent(dialog, new Event('cancel', { cancelable: true }))
    await fireEvent(dialog, new Event('cancel', { cancelable: true }))
    expect(emitted().cancel).toBeUndefined()
    expect(dialog).toBeVisible()
    await rerender({ busy: false, error: 'The passkey was not removed. Confirm your identity and try again.' })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus())
    expect(screen.getByRole('alert')).toHaveTextContent('The passkey was not removed.')
  })
})
