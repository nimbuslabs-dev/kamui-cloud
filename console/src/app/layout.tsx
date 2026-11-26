import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Kamui Cloud - Mobile Workspace',
  description: 'Your lab. Your cloud. Code from anywhere.',
};

export const viewport = {
  width: 'device-width',
  initialScale: 1,
  themeColor: '#6366f1',
  other: {
    'apple-mobile-web-app-capable': 'yes',
    'apple-mobile-web-app-status-bar-style': 'default',
    'apple-mobile-web-app-title': 'Kamui Cloud',
  },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className="antialiased">
      <head>
        <meta name="mobile-web-app-capable" content="yes" />
        <meta name="apple-mobile-web-app-capable" content="yes" />
        <link rel="manifest" href="/manifest.json" />
        <link rel="apple-touch-icon" href="/icon.svg" />
      </head>
      <body className="bg-neutral-50 dark:bg-neutral-950 text-neutral-900 dark:text-neutral-100 min-h-screen">
        {children}
      </body>
    </html>
  );
}