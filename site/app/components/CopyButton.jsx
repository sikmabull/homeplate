'use client';

import { useState } from 'react';

export default function CopyButton({ text, big = false }) {
  const [copied, setCopied] = useState(false);
  const onCopy = () => {
    const done = () => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    };
    try { navigator.clipboard.writeText(text).then(done, done); } catch (e) { done(); }
  };
  return (
    <button className="copy" onClick={onCopy} aria-label={`Copy: ${text}`}>
      <span className="prompt">$</span>
      <span>{text}</span>
      <span className="tag">{copied ? 'copied' : 'copy'}</span>
    </button>
  );
}
