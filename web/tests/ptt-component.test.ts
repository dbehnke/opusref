import { fireEvent, render, screen } from '@testing-library/vue'
import { nextTick } from 'vue'
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it } from 'vitest'
import PTTControl from '../src/components/PTTControl.vue'
beforeEach(() => localStorage.clear())
describe('PTT control', () => {
  it('has a 44 pixel control and visible latch label', async () => { const { emitted } = render(PTTControl); const button = screen.getByRole('button', { name: 'Push to talk' }); expect(screen.getByLabelText('Latch PTT')).toBeVisible(); await fireEvent.pointerDown(button); expect(emitted().request).toHaveLength(1); await fireEvent.pointerUp(button); expect(emitted().stop).toHaveLength(1) })
  it('stores latch preference per device', async () => { render(PTTControl); await fireEvent.click(screen.getByLabelText('Latch PTT')); expect(localStorage.getItem('opusref.ptt.latched')).toBe('true') })
  it('explains and announces latch safety behavior', async () => {
    render(PTTControl)
    expect(screen.getByText('Hold the button to transmit. Release it to stop.')).toBeVisible()
    await fireEvent.click(screen.getByLabelText('Latch PTT'))
    expect(screen.getByText('Latch mode is on. Activate Push to talk once to start. Activate it again to stop.')).toBeVisible()
    expect(screen.getByRole('status')).toHaveTextContent('Latch mode is on. Release will not stop transmission.')
    await fireEvent.click(screen.getByLabelText('Latch PTT'))
    expect(screen.getByRole('status')).toHaveTextContent('Latch mode is off. Release stops transmission.')
  })
  it('stops hold mode on pointer cancellation', async () => { const { emitted } = render(PTTControl); const button = screen.getByRole('button', { name: 'Push to talk' }); await fireEvent.pointerDown(button); await fireEvent.pointerCancel(button); expect(emitted().stop).toHaveLength(1) })
  it('uses Space only while the focused control is held', async () => { const { emitted } = render(PTTControl); const button = screen.getByRole('button', { name: 'Push to talk' }); button.focus(); await fireEvent.keyDown(button, { code: 'Space' }); expect(emitted().request).toHaveLength(1); await fireEvent.keyUp(button, { code: 'Space' }); expect(emitted().stop).toHaveLength(1) })
  it('announces the requesting, transmitting, stopping, and TOT states', async () => {
    const wrapper = mount(PTTControl, { props: { remaining: 180 } })
    const button = wrapper.get('button')
    await button.trigger('pointerdown')
    expect(button.text()).toContain('Requesting')
    expect(wrapper.get('[role="status"]').text()).toBe('Requesting the reflector floor.')
    ;(wrapper.vm as unknown as { granted(): void }).granted(); await nextTick()
    expect(button.text()).toContain('Transmitting · 180 s')
    expect(wrapper.get('[role="status"]').text()).toBe('Transmission started.')
    await button.trigger('pointerup')
    expect(button.text()).toContain('Stopping')
    expect(wrapper.get('[role="status"]').text()).toBe('Stopping transmission.')
    ;(wrapper.vm as unknown as { ended(): void }).ended(); await nextTick()
    expect(wrapper.get('[role="status"]').text()).toBe('Transmission stopped.')
  })
})
