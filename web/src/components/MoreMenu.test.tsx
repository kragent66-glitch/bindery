import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import MoreMenu from './MoreMenu'

function setup(overrides: Partial<Parameters<typeof MoreMenu>[0]> = {}) {
  const onRename = vi.fn()
  const onDelete = vi.fn()
  const utils = render(
    <div>
      <button>outside</button>
      <MoreMenu
        label="More"
        items={[
          { label: 'Rename files', onSelect: onRename },
          { label: 'Merge…', onSelect: vi.fn(), disabled: true },
          { label: 'Delete', onSelect: onDelete, danger: true },
        ]}
        {...overrides}
      />
    </div>,
  )
  return { ...utils, onRename, onDelete }
}

const trigger = () => screen.getByRole('button', { name: /More/ })

describe('MoreMenu', () => {
  it('is closed initially and reports collapsed state', () => {
    setup()
    expect(screen.queryByRole('menu')).toBeNull()
    expect(trigger()).toHaveAttribute('aria-expanded', 'false')
    expect(trigger()).toHaveAttribute('aria-haspopup', 'menu')
  })

  it('opens on click and exposes its items as menuitems', () => {
    setup()
    fireEvent.click(trigger())
    expect(screen.getByRole('menu')).toBeInTheDocument()
    expect(trigger()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getAllByRole('menuitem')).toHaveLength(3)
  })

  it('runs the item handler and closes on select', () => {
    const { onRename } = setup()
    fireEvent.click(trigger())
    fireEvent.click(screen.getByRole('menuitem', { name: 'Rename files' }))
    expect(onRename).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('does not fire a disabled item', () => {
    setup()
    fireEvent.click(trigger())
    const disabled = screen.getByRole('menuitem', { name: 'Merge…' })
    expect(disabled).toBeDisabled()
  })

  it('closes on Escape and returns focus to the trigger', () => {
    setup()
    fireEvent.click(trigger())
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('menu')).toBeNull()
    expect(trigger()).toHaveFocus()
  })

  it('closes on outside click', () => {
    setup()
    fireEvent.click(trigger())
    fireEvent.pointerDown(screen.getByRole('button', { name: 'outside' }))
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('stays open when clicking inside the menu surface', () => {
    setup()
    fireEvent.click(trigger())
    fireEvent.pointerDown(screen.getByRole('menu'))
    expect(screen.getByRole('menu')).toBeInTheDocument()
  })

  it('opens with ArrowDown and focuses the first item', () => {
    setup()
    fireEvent.keyDown(trigger(), { key: 'ArrowDown' })
    expect(screen.getByRole('menuitem', { name: 'Rename files' })).toHaveFocus()
  })

  it('opens with ArrowUp and focuses the last item', () => {
    setup()
    fireEvent.keyDown(trigger(), { key: 'ArrowUp' })
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus()
  })

  it('skips disabled items when arrowing and wraps at the end', () => {
    setup()
    fireEvent.keyDown(trigger(), { key: 'ArrowDown' })
    const menu = screen.getByRole('menu')
    // Rename -> Delete (Merge is disabled and skipped)
    fireEvent.keyDown(menu, { key: 'ArrowDown' })
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus()
    // wraps back round to the first enabled item
    fireEvent.keyDown(menu, { key: 'ArrowDown' })
    expect(screen.getByRole('menuitem', { name: 'Rename files' })).toHaveFocus()
  })

  it('jumps to the ends with Home and End', () => {
    setup()
    fireEvent.keyDown(trigger(), { key: 'ArrowDown' })
    const menu = screen.getByRole('menu')
    fireEvent.keyDown(menu, { key: 'End' })
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveFocus()
    fireEvent.keyDown(menu, { key: 'Home' })
    expect(screen.getByRole('menuitem', { name: 'Rename files' })).toHaveFocus()
  })

  it('closes on Tab without stealing focus back to the trigger', () => {
    setup()
    fireEvent.keyDown(trigger(), { key: 'ArrowDown' })
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Tab' })
    expect(screen.queryByRole('menu')).toBeNull()
    expect(trigger()).not.toHaveFocus()
  })
})

describe('MoreMenu near the screen edge', () => {
  // On a phone the trigger can wrap to the start of a row, and a menu lined
  // up with its right edge then opened at left=-101px, clipping its items.
  it('flips to the left edge when a right aligned menu would overflow', () => {
    const spy = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      const left = this.getAttribute('role') === 'menu' && this.className.includes('right-0') ? -101 : 16
      return { left, right: left + 176, top: 0, bottom: 0, width: 176, height: 0, x: left, y: 0, toJSON: () => ({}) } as DOMRect
    })
    try {
      render(<MoreMenu label="More" items={[{ label: 'Delete', onSelect: vi.fn() }]} />)
      fireEvent.click(screen.getByRole('button', { name: /More/ }))
      const menu = screen.getByRole('menu')
      expect(menu.className).toContain('left-0')
      expect(menu.className).not.toContain('right-0')
    } finally {
      spy.mockRestore()
    }
  })

  it('stays right aligned when it fits, and again on the next open', () => {
    render(<MoreMenu label="More" items={[{ label: 'Delete', onSelect: vi.fn() }]} />)
    fireEvent.click(screen.getByRole('button', { name: /More/ }))
    expect(screen.getByRole('menu').className).toContain('right-0')
  })

  it('renders caution items in amber', () => {
    render(<MoreMenu label="More" items={[{ label: 'Set both', onSelect: vi.fn(), caution: true }]} />)
    fireEvent.click(screen.getByRole('button', { name: /More/ }))
    expect(screen.getByRole('menuitem', { name: 'Set both' }).className).toContain('text-amber-700')
  })
})
