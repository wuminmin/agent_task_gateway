SELECT session_user,current_user,r.rolsuper,r.rolbypassrls FROM pg_roles r WHERE r.rolname=current_user;
SELECT n.nspname,c.relname,c.relrowsecurity,c.relforcerowsecurity,pg_get_userbyid(c.relowner)=current_user AS role_is_owner FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='reporting' AND c.relname='orders';
SELECT schemaname,tablename,policyname,roles,cmd,qual,with_check FROM pg_policies WHERE schemaname='reporting' AND tablename='orders' ORDER BY policyname;
SELECT roleid::regrole,member::regrole,admin_option FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user) ORDER BY 1::text;
