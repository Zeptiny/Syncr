import { Link, useLocation } from "@tanstack/react-router";
import { LogIn } from "lucide-react";

import { Logo, SkipLink, ThemeToggle } from "@/components/app/app-shell";
import { Button } from "@/components/ui/button";

import { SearchButton } from "./docs-search";

/**
 * PublicDocsLayout frames the docs for readers who are not signed in: a slim
 * header (docs home, Search, theme, Sign in) above the page.
 */
export function PublicDocsLayout({ children }: { children: React.ReactNode }) {
  const href = useLocation({ select: (l) => l.href });
  return (
    <div className="min-h-svh bg-background [--docs-scroll-margin:5rem] [--docs-sticky-top:5rem]">
      <SkipLink />
      <header className="sticky top-0 z-30 border-b bg-background/90 backdrop-blur-sm">
        <div className="mx-auto flex h-14 w-full max-w-[1200px] items-center gap-2 px-4 sm:px-8">
          <Link
            to="/docs"
            className="flex min-w-0 items-center gap-2.5"
            aria-label="Syncr docs home"
          >
            <Logo />
            <span className="truncate text-[16px] font-semibold tracking-[-0.01em]">
              Syncr docs
            </span>
          </Link>
          <div className="ml-auto flex items-center gap-1.5">
            <SearchButton compact className="sm:w-44" />
            <ThemeToggle />
            <Button asChild size="sm">
              <Link to="/login" search={{ redirect: href }}>
                <LogIn aria-hidden />
                Sign in
              </Link>
            </Button>
          </div>
        </div>
      </header>
      <main
        id="main-content"
        tabIndex={-1}
        className="mx-auto w-full max-w-[1200px] px-4 py-6 outline-none sm:px-8 sm:py-8"
      >
        {children}
      </main>
    </div>
  );
}
