SELECT session_user, current_user, r.rolcanlogin, r.rolsuper, r.rolinherit,
       r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls
FROM pg_roles AS r
WHERE r.rolname = current_user;

SELECT n.nspname, c.relname, c.relrowsecurity, c.relforcerowsecurity,
       owner.rolname AS table_owner
FROM pg_class AS c
JOIN pg_namespace AS n ON n.oid = c.relnamespace
JOIN pg_roles AS owner ON owner.oid = c.relowner
WHERE n.nspname = 'final_v5_rls' AND c.relname = 'expense_detail';

SELECT schemaname, tablename, policyname, permissive, roles, cmd, qual, with_check
FROM pg_policies
WHERE schemaname = 'final_v5_rls' AND tablename = 'expense_detail'
ORDER BY policyname;

SELECT granted.rolname AS granted_role, member.rolname AS member_role,
       grantor.rolname AS grantor_role, membership.admin_option,
       membership.inherit_option, membership.set_option
FROM pg_auth_members AS membership
JOIN pg_roles AS granted ON granted.oid = membership.roleid
JOIN pg_roles AS member ON member.oid = membership.member
JOIN pg_roles AS grantor ON grantor.oid = membership.grantor
WHERE granted.rolname = 'final_v5_rls_reader' OR member.rolname = 'final_v5_rls_reader'
ORDER BY granted_role, member_role, grantor_role;
