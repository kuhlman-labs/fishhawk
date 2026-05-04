import { Route, Routes } from 'react-router';
import { AuthProvider } from './auth/auth-provider';
import { RequireAuth } from './auth/require-auth';
import { Root } from './routes/root';
import { Login } from './routes/login';
import { Runs } from './routes/runs';
import { Audit } from './routes/audit';
import { NotFound } from './routes/not-found';

export function App() {
  return (
    <AuthProvider>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route
          path="/"
          element={
            <RequireAuth>
              <Root />
            </RequireAuth>
          }
        >
          <Route index element={<Runs />} />
          <Route path="runs" element={<Runs />} />
          <Route path="audit" element={<Audit />} />
        </Route>
        <Route path="*" element={<NotFound />} />
      </Routes>
    </AuthProvider>
  );
}
