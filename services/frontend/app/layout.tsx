import "./globals.css";
import type { Metadata } from "next";
import { Providers, SessionGuard } from "./providers";
import { HeaderBar } from "./header-bar";
import { Tagline } from "./tagline";
import { noFlashScript } from "../lib/preferences";

export const metadata: Metadata = {
  title: "Can I Haz Kubernetes",
  description: "A cloud-native image board running on Kubernetes",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" data-theme="light" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: noFlashScript }} />
        <link rel="preconnect" href="https://fonts.googleapis.com" />
        <link rel="preconnect" href="https://fonts.gstatic.com" crossOrigin="" />
        <link
          href="https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,600;12..96,800&family=Inter:wght@400;500;600&family=JetBrains+Mono:wght@400&display=swap"
          rel="stylesheet"
        />
      </head>
      <body>
        <Providers>
          <HeaderBar />
          <div className="board-nav">
            <Tagline />
          </div>
          <main><SessionGuard>{children}</SessionGuard></main>
          <footer className="site-footer">
            <a className="source-link" href="https://github.com/codemowers/lolcatz" target="_blank" rel="noopener noreferrer" aria-label="codemowers/lolcatz on GitHub">
              <span className="github-logo" aria-hidden="true" /> codemowers/lolcatz
            </a>
          </footer>
        </Providers>
      </body>
    </html>
  );
}
