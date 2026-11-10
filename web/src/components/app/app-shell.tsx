import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import {
  Archive,
  ArrowRightLeft,
  Bell,
  CalendarClock,
  ChevronsUpDown,
  Cloud,
  LayoutDashboard,
  LogOut,
  Monitor,
  Moon,
  Play,
  Server,
  Settings,
  Sun,
  Users,
  type LucideIcon,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar";
import { AuthService } from "@/gen/syncr/v1/auth_pb";
import { displayName, endSession, useSession } from "@/hooks/use-session";
import { useTheme, type Theme } from "@/lib/theme";
import { cn } from "@/lib/utils";

interface NavItem {
  to:
    | "/"
    | "/jobs"
    | "/schedules"
    | "/remotes"
    | "/repositories"
    | "/workers"
    | "/notifications"
    | "/users";
  label: string;
  icon: LucideIcon;
  adminOnly?: boolean;
}

const mainNav: NavItem[] = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard },
  { to: "/jobs", label: "Jobs", icon: ArrowRightLeft },
  { to: "/schedules", label: "Schedules", icon: CalendarClock },
  { to: "/remotes", label: "Remotes", icon: Cloud },
  { to: "/repositories", label: "Repositories", icon: Archive },
  { to: "/workers", label: "Workers", icon: Server },
  { to: "/notifications", label: "Notifications", icon: Bell },
];

const adminNav: NavItem[] = [{ to: "/users", label: "Users", icon: Users, adminOnly: true }];

/** Logo is the Syncr mark: two opposed arrows in a rounded square. */
export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" aria-hidden className={cn("size-7", className)}>
      <rect width="32" height="32" rx="8" className="fill-primary" />
      <path
        d="M9 12.5h11.5l-3-3M23 19.5H11.5l3 3"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.4"
        strokeLinecap="round"
        strokeLinejoin="round"
        className="text-primary-foreground"
      />
    </svg>
  );
}

function NavLinks({ items }: { items: NavItem[] }) {
  const { pathname } = useLocation();
  const { setOpenMobile } = useSidebar();
  return (
    <SidebarMenu>
      {items.map((it) => {
        const active =
          it.to === "/" ? pathname === "/" : pathname === it.to || pathname.startsWith(`${it.to}/`);
        return (
          <SidebarMenuItem key={it.to}>
            <SidebarMenuButton
              asChild
              isActive={active}
              className="relative h-9 before:absolute before:top-2 before:bottom-2 before:-left-2 before:w-[3px] before:rounded-r-full data-[active=true]:before:bg-sidebar-primary"
            >
              <Link to={it.to} onClick={() => setOpenMobile(false)}>
                <it.icon aria-hidden />
                <span>{it.label}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        );
      })}
    </SidebarMenu>
  );
}

const themeIcons: Record<Theme, LucideIcon> = { light: Sun, dark: Moon, system: Monitor };

/** ThemeToggle cycles light → dark → system. */
export function ThemeToggle({ className }: { className?: string }) {
  const { theme, setTheme } = useTheme();
  const next: Theme = theme === "light" ? "dark" : theme === "dark" ? "system" : "light";
  const Icon = themeIcons[theme];
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      className={className}
      onClick={() => setTheme(next)}
      aria-label={`Theme: ${theme}. Switch to ${next}`}
      title={`Theme: ${theme}`}
    >
      <Icon aria-hidden />
    </Button>
  );
}

function UserMenu() {
  const { user, isAdmin } = useSession();
  const { theme, setTheme } = useTheme();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const logout = useMutation(AuthService.method.logout);
  const name = displayName(user);
  const initials = name
    .split(/[\s._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase())
    .join("");

  const signOut = async () => {
    try {
      await logout.mutateAsync({});
    } finally {
      endSession(queryClient);
      await navigate({ to: "/login" });
    }
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <SidebarMenuButton size="lg" className="data-[state=open]:bg-sidebar-accent">
          <span className="grid size-8 shrink-0 place-items-center rounded-full bg-sidebar-accent text-xs font-semibold text-sidebar-accent-foreground">
            {initials || "?"}
          </span>
          <span className="grid min-w-0 flex-1 text-left leading-tight">
            <span className="truncate text-[13px] font-medium text-sidebar-accent-foreground">
              {name}
            </span>
            <span className="truncate text-xs opacity-75">
              {isAdmin ? "Administrator" : "User"}
            </span>
          </span>
          <ChevronsUpDown className="ml-auto opacity-60" aria-hidden />
        </SidebarMenuButton>
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start" className="w-60">
        <DropdownMenuLabel className="font-normal">
          <div className="text-[13px] font-medium">{name}</div>
          <div className="text-xs text-muted-foreground">{user?.username}</div>
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
          Theme
        </DropdownMenuLabel>
        <DropdownMenuRadioGroup value={theme} onValueChange={(v) => setTheme(v as Theme)}>
          <DropdownMenuRadioItem value="light">Light</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark">Dark</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="system">Match system</DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/settings">
            <Settings aria-hidden />
            Settings
          </Link>
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => void signOut()}>
          <LogOut aria-hidden />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * AppShell is the signed-in layout: the sidebar (navigation, run-job
 * shortcut, user menu and theme) and the page area.
 */
export function AppShell({ children }: { children: React.ReactNode }) {
  const { isAdmin } = useSession();
  return (
    <SidebarProvider>
      <Sidebar className="border-r border-sidebar-border">
        <SidebarHeader className="gap-4 px-4 pt-5 pb-2">
          <Link to="/" className="flex items-center gap-2.5" aria-label="Syncr home">
            <Logo />
            <span className="text-[17px] font-semibold tracking-[-0.01em] text-sidebar-accent-foreground">
              Syncr
            </span>
          </Link>
          <Button
            asChild
            className="w-full justify-start border border-sidebar-border bg-sidebar-accent text-sidebar-accent-foreground hover:bg-sidebar-accent/70 [&_svg]:text-sidebar-primary"
          >
            <Link to="/jobs/new">
              <Play aria-hidden />
              Run a job
            </Link>
          </Button>
        </SidebarHeader>
        <SidebarContent className="px-2">
          <SidebarGroup>
            <SidebarGroupContent>
              <NavLinks items={mainNav} />
            </SidebarGroupContent>
          </SidebarGroup>
          {isAdmin && (
            <SidebarGroup>
              <SidebarGroupLabel className="text-sidebar-foreground/60">
                Administration
              </SidebarGroupLabel>
              <SidebarGroupContent>
                <NavLinks items={adminNav} />
              </SidebarGroupContent>
            </SidebarGroup>
          )}
        </SidebarContent>
        <SidebarFooter className="border-t border-sidebar-border p-2">
          <div className="flex items-center gap-1">
            <SidebarMenu className="min-w-0 flex-1">
              <SidebarMenuItem>
                <UserMenu />
              </SidebarMenuItem>
            </SidebarMenu>
            <ThemeToggle className="text-sidebar-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground" />
          </div>
        </SidebarFooter>
      </Sidebar>
      <SidebarInset className="min-w-0 bg-background">
        <header className="sticky top-0 z-20 flex h-12 items-center gap-2 bg-sidebar px-3 text-sidebar-foreground md:hidden">
          <SidebarTrigger className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground" />
          <Link to="/" className="flex items-center gap-2" aria-label="Syncr home">
            <Logo className="size-6" />
            <span className="font-semibold text-sidebar-accent-foreground">Syncr</span>
          </Link>
        </header>
        <div className="mx-auto w-full max-w-[1200px] px-4 py-6 sm:px-8 sm:py-8">{children}</div>
      </SidebarInset>
    </SidebarProvider>
  );
}
