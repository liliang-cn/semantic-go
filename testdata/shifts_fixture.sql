-- Fixture for the grain-safety regression set. Sized so every expected answer
-- can be worked out by hand and checked against the SQL the compiler emits.
--
-- The trap is deliberate and lives in shift_pay: guard g1's shift s1 has TWO
-- pay components, so any query that joins shifts to pay before aggregating
-- counts its 8 hours twice.
CREATE TABLE guards (guard_id TEXT, region TEXT, name TEXT);
INSERT INTO guards VALUES
  ('g1','North','Ana'), ('g2','North','Ben'), ('g3','South','Cho');

CREATE TABLE guard_shifts (guard_id TEXT, shift_id TEXT, shift_date DATE, shift_type TEXT, hours_worked DOUBLE);
INSERT INTO guard_shifts VALUES
  ('g1','s1',DATE '2026-08-03','night',8),
  ('g1','s2',DATE '2026-08-04','day',  6),
  ('g2','s3',DATE '2026-08-05','night',9),
  ('g3','s4',DATE '2026-08-06','night',6),
  ('g3','s5',DATE '2026-09-01','night',5);   -- outside August

CREATE TABLE shift_pay (guard_id TEXT, shift_id TEXT, pay_component_id TEXT, component TEXT, amount DOUBLE);
INSERT INTO shift_pay VALUES
  ('g1','s1','pc1','base',    40000),
  ('g1','s1','pc2','penalty', 20000),   -- the second row that fans s1 out
  ('g1','s2','pc1','base',     3000),
  ('g2','s3','pc1','base',    15000),
  ('g3','s4','pc1','base',     1000),
  ('g3','s5','pc1','base',      900);

CREATE TABLE sites (site_id TEXT, category TEXT);
INSERT INTO sites VALUES ('si1','retail'), ('si2','industrial');

CREATE TABLE site_assignments (assignment_id TEXT, shift_id TEXT, site_id TEXT);
INSERT INTO site_assignments VALUES
  ('a1','s1','si1'), ('a2','s1','si2'), ('a3','s3','si1');

CREATE TABLE clients (client_id TEXT, name TEXT);
INSERT INTO clients VALUES ('c1','Acme');

CREATE TABLE invoices (invoice_id TEXT, client_id TEXT, amount DOUBLE);
INSERT INTO invoices VALUES ('i1','c1',99000);
