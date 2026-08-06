'use client';

import { useEffect, useState } from 'react';

// Animated "saved this month" ledger. Eases up to the target, then drifts
// upward slowly - the counter on a real machine never sits still either.
export default function SavingsCounter({ target = 342.8 }) {
  const [val, setVal] = useState(0);

  useEffect(() => {
    let raf, drift;
    const t0 = performance.now();
    const dur = 1500;
    const tick = (t) => {
      const p = Math.min(1, (t - t0) / dur);
      setVal(target * (1 - Math.pow(1 - p, 3)));
      if (p < 1) raf = requestAnimationFrame(tick);
      else drift = setInterval(() => setVal((v) => v + 0.004 + Math.random() * 0.02), 2600);
    };
    raf = requestAnimationFrame(tick);
    return () => { cancelAnimationFrame(raf); clearInterval(drift); };
  }, [target]);

  const amount = '$' + val.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  return (
    <div className="savings" aria-label={`${amount} saved this month`}>
      {amount.split('').map((c, i) =>
        /[0-9]/.test(c) ? (
          <div key={i} className="digit">{c}</div>
        ) : (
          <div key={i} className="plain">{c}</div>
        )
      )}
    </div>
  );
}
