import type * as React from "react"
import { Navigate, Route, Routes, useLocation } from "react-router-dom"

import { AppShell } from "@/components/app-shell"
import { Booting } from "@/components/page"
import { useAuth } from "@/context/auth"
import AdminOverviewPage from "@/pages/admin-overview"
import ApiKeysPage from "@/pages/api-keys"
import ConnectionsPage from "@/pages/connections"
import DashboardPage from "@/pages/dashboard"
import LoginPage from "@/pages/login"
import NotFoundPage from "@/pages/not-found"
import QuotaPage from "@/pages/quota"
import SettingsPage from "@/pages/settings"
import SetupPage from "@/pages/setup"
import UsagePage from "@/pages/usage"
import UserDetailPage from "@/pages/user-detail"
import UsersPage from "@/pages/users"

/** Sends the visitor to the sign-in page unless the boot-time refresh found a
 * session. */
function RequireAuth({ children }: { children: React.ReactNode }) {
  const { booting, user } = useAuth()
  const location = useLocation()

  if (booting) {
    return <Booting />
  }
  if (!user) {
    return (
      <Navigate
        to="/login"
        replace
        state={{ from: location.pathname + location.search }}
      />
    )
  }
  return <>{children}</>
}

/** Admin pages are guarded in the UI as well as on the server. */
function RequireAdmin({ children }: { children: React.ReactNode }) {
  const { booting, isAdmin } = useAuth()

  if (booting) {
    return <Booting />
  }
  if (!isAdmin) {
    return <Navigate to="/" replace />
  }
  return <>{children}</>
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      {/* Both live outside RequireAuth, and each sends the visitor to the other
          when it is the wrong one: a fresh install hits / → /login →
          needs_setup → /setup, and /setup bounces back once an account
          exists. */}
      <Route path="/setup" element={<SetupPage />} />
      <Route
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route path="/" element={<DashboardPage />} />
        <Route path="/connections" element={<ConnectionsPage />} />
        <Route path="/keys" element={<ApiKeysPage />} />
        <Route path="/usage" element={<UsagePage />} />
        <Route path="/quota" element={<QuotaPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route
          path="/admin"
          element={
            <RequireAdmin>
              <AdminOverviewPage />
            </RequireAdmin>
          }
        />
        <Route
          path="/admin/users"
          element={
            <RequireAdmin>
              <UsersPage />
            </RequireAdmin>
          }
        />
        <Route
          path="/admin/users/:id"
          element={
            <RequireAdmin>
              <UserDetailPage />
            </RequireAdmin>
          }
        />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  )
}

export default App
