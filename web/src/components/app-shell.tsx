import * as React from "react"
import { NavLink, Outlet, useLocation } from "react-router-dom"
import {
  ActivityIcon,
  GaugeIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  LogOutIcon,
  MenuIcon,
  MonitorIcon,
  MoonIcon,
  PlugZapIcon,
  ServerIcon,
  SettingsIcon,
  SunIcon,
  UsersIcon,
  XIcon,
} from "lucide-react"

import { useTheme } from "@/components/theme-provider"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { useAuth } from "@/context/auth"
import { cn } from "@/lib/utils"

type NavItem = {
  to: string
  label: string
  icon: React.ComponentType<{ className?: string }>
  admin?: boolean
  end?: boolean
}

const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboardIcon, end: true },
  { to: "/connections", label: "Connections", icon: PlugZapIcon },
  { to: "/keys", label: "API keys", icon: KeyRoundIcon },
  { to: "/usage", label: "Usage", icon: ActivityIcon },
  { to: "/quota", label: "Quota", icon: GaugeIcon },
  { to: "/settings", label: "Settings", icon: SettingsIcon },
]

const ADMIN_NAV: NavItem[] = [
  { to: "/admin", label: "Server", icon: ServerIcon, admin: true, end: true },
  { to: "/admin/users", label: "Users", icon: UsersIcon, admin: true },
]

function NavItems({ items, onNavigate }: { items: NavItem[]; onNavigate?: () => void }) {
  return (
    <nav className="grid gap-0.5">
      {items.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          onClick={onNavigate}
          className={({ isActive }) =>
            cn(
              "flex h-8 items-center gap-2 rounded-lg px-2 text-sm font-medium transition-colors",
              isActive
                ? "bg-muted text-foreground"
                : "text-muted-foreground hover:bg-muted/60 hover:text-foreground"
            )
          }
        >
          <item.icon className="size-4 shrink-0" />
          {item.label}
        </NavLink>
      ))}
    </nav>
  )
}

function Brand() {
  return (
    <div className="flex h-12 items-center gap-2 px-3">
      <div className="flex size-6 items-center justify-center rounded-md bg-primary text-[11px] font-bold text-primary-foreground">
        ah
      </div>
      <span className="text-sm font-semibold tracking-tight">aihub</span>
    </div>
  )
}

function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const next = theme === "light" ? "dark" : theme === "dark" ? "system" : "light"
  const Icon = theme === "light" ? SunIcon : theme === "dark" ? MoonIcon : MonitorIcon

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      title={`Theme: ${theme} (press d to toggle)`}
      aria-label="Change theme"
      onClick={() => setTheme(next)}
    >
      <Icon className="size-4" />
    </Button>
  )
}

/** Sidebar, topbar and the routed page body. */
function AppShell() {
  const { user, isAdmin, logout } = useAuth()
  const [drawer, setDrawer] = React.useState(false)
  const location = useLocation()

  React.useEffect(() => {
    setDrawer(false)
  }, [location.pathname])

  const sidebar = (
    <div className="flex h-full flex-col">
      <Brand />
      <div className="flex-1 overflow-y-auto px-2 pb-3">
        <NavItems items={NAV} />
        {isAdmin ? (
          <>
            <p className="mt-4 mb-1 px-2 text-xs font-medium text-muted-foreground">
              Administration
            </p>
            <NavItems items={ADMIN_NAV} />
          </>
        ) : null}
      </div>
      <div className="border-t border-border p-2">
        <div className="grid gap-1 rounded-lg px-2 py-1.5">
          <span className="truncate text-sm font-medium" title={user?.username}>
            {user?.display_name || user?.username}
          </span>
          <span className="flex items-center gap-1.5">
            <Badge variant={isAdmin ? "default" : "muted"}>
              {user?.role ?? "user"}
            </Badge>
            <span className="truncate text-xs text-muted-foreground">
              {user?.username}
            </span>
          </span>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="mt-1 w-full justify-start"
          onClick={() => void logout()}
        >
          <LogOutIcon />
          Sign out
        </Button>
      </div>
    </div>
  )

  return (
    <div className="flex min-h-svh bg-background text-foreground">
      <aside className="hidden w-56 shrink-0 border-r border-border md:block">
        <div className="sticky top-0 h-svh">{sidebar}</div>
      </aside>

      {drawer ? (
        <div
          className="fixed inset-0 z-40 bg-black/45 md:hidden"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) {
              setDrawer(false)
            }
          }}
        >
          <div className="h-full w-60 border-r border-border bg-background">
            {sidebar}
          </div>
        </div>
      ) : null}

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-30 flex h-12 shrink-0 items-center gap-2 border-b border-border bg-background/85 px-3 backdrop-blur">
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Menu"
            className="md:hidden"
            onClick={() => setDrawer((open) => !open)}
          >
            {drawer ? <XIcon /> : <MenuIcon />}
          </Button>
          <span className="text-sm font-medium md:hidden">aihub</span>
          <div className="flex-1" />
          <a
            href="/v1/models"
            target="_blank"
            rel="noreferrer"
            className="hidden text-xs text-muted-foreground hover:text-foreground sm:block"
          >
            /v1 endpoint
          </a>
          <ThemeToggle />
        </header>
        <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-5">
          <Outlet />
        </main>
      </div>
    </div>
  )
}

export { AppShell }
