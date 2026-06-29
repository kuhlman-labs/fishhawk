import { Route, Routes } from 'react-router';
import { AuthProvider } from './auth/auth-provider';
import { RequireAuth } from './auth/require-auth';
import { Root } from './routes/root';
import { Login } from './routes/login';
import { Runs } from './routes/runs';
import { RunDetail } from './routes/run-detail';
import { StageDetail } from './routes/stage-detail';
import { Campaigns } from './routes/campaigns';
import { CampaignDetail } from './routes/campaign-detail';
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
          <Route path="runs/:runId" element={<RunDetail />} />
          <Route path="runs/:runId/stages/:stageId" element={<StageDetail />} />
          <Route path="campaigns" element={<Campaigns />} />
          <Route path="campaigns/:campaignId" element={<CampaignDetail />} />
          <Route path="audit" element={<Audit />} />
        </Route>
        <Route path="*" element={<NotFound />} />
      </Routes>
    </AuthProvider>
  );
}
