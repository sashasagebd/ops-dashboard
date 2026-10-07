import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Sparkline } from './Sparkline'

describe('Sparkline', () => {
  const pct = (v: number) => `${v.toFixed(1)}%`

  it('names itself with the average and peak, and draws both lines', () => {
    const { container } = render(
      <Sparkline values={[10, 20]} peaks={[12, 80]} max={100} label="mc CPU, last hour" format={pct} />,
    )

    expect(screen.getByRole('img', { name: 'mc CPU, last hour: average 15.0%, peak 80.0%' })).toBeInTheDocument()
    expect(container.querySelector('.sparkline-line')).toHaveAttribute('d', 'M0 20L100 18')
    expect(container.querySelector('.sparkline-peaks')).toHaveAttribute('d', 'M0 19.6L100 6')
  })

  it('says when there is no data yet', () => {
    const { container } = render(<Sparkline values={[null, null]} max={100} label="Host CPU" format={pct} />)

    expect(screen.getByRole('img', { name: 'Host CPU: no data yet' })).toBeInTheDocument()
    expect(container.querySelector('.sparkline-peaks')).not.toBeInTheDocument()
  })
})
