'use client';

import { useState } from 'react';

// Terminal-card line with a click-to-copy affordance.
export default function CopyLine({ text, children }) {
  const [copied, setCopied] = useState(false);
  const onCopy = () => {
    const done = () => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    };
    try { navigator.clipboard.writeText(text).then(done, done); } catch (e) { done(); }
  };
  return (
    <button className="inline" onClick={onCopy} aria-label={`Copy: ${text}`}>
      <span className="prompt">$</span>
      <span>{children || text}</span>
      <span className="minilabel">{copied ? 'copied' : 'copy'}</span>
    </button>
  );
}
