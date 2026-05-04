import { Route, Routes } from 'react-router';
import { Root } from './routes/root';
import { Login } from './routes/login';
import { Runs } from './routes/runs';
import { Audit } from './routes/audit';
import { NotFound } from './routes/not-found';

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<Root />}>
        <Route index element={<Runs />} />
        <Route path="runs" element={<Runs />} />
        <Route path="audit" element={<Audit />} />
      </Route>
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
