import { createFileRoute } from "@tanstack/react-router";
import { NoPermissionView } from "@/components/noPermissionView";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import HeadroomPage from "./page";

function RouteComponent() {
	const allowed = useRbac(RbacResource.Plugins, RbacOperation.View);
	return allowed ? <HeadroomPage /> : <NoPermissionView entity="Headroom" />;
}

export const Route = createFileRoute("/workspace/headroom")({ component: RouteComponent });