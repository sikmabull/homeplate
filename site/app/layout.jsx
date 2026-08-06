import './globals.css';

export const metadata = {
  title: 'Homeplate — Bring your runners home',
  description:
    'GitHub Actions on your own machine. Green checks on your PRs, up to 31x cheaper than hosted macOS, and it keeps working when GitHub doesn’t.',
  icons: {
    icon: [
      { url: '/assets/favicon.svg', type: 'image/svg+xml' },
      { url: '/assets/favicon-32.png', sizes: '32x32', type: 'image/png' },
      { url: '/assets/favicon-16.png', sizes: '16x16', type: 'image/png' },
    ],
    apple: '/assets/apple-touch-icon-180.png',
  },
  openGraph: {
    title: 'Homeplate — Bring your runners home',
    description: 'Your machine as your GitHub Actions CI.',
    type: 'website',
  },
};

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
