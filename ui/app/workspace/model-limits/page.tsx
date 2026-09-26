import ModelLimitsView from "./views/modelLimitsView";
import SubscriptionLimits from "./views/subscriptionLimits";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { parseAsStringLiteral, useQueryState } from "nuqs";

export default function ModelLimitsPage() {
	const [tab, setTab] = useQueryState("tab", parseAsStringLiteral(["governance", "subscriptions"] as const).withDefault("governance"));
	return (
		<div className="no-padding-parent mx-auto flex h-[calc(var(--app-content-viewport)_-_var(--app-bottom-padding))] min-h-0 w-full flex-col overflow-hidden p-4">
			<Tabs value={tab} onValueChange={(value) => setTab(value as "governance" | "subscriptions")} className="flex min-h-0 flex-1 flex-col">
				<TabsList>
					<TabsTrigger value="governance">Budgets & Rate Limits</TabsTrigger>
					<TabsTrigger value="subscriptions">Subscription Limits</TabsTrigger>
				</TabsList>
				<TabsContent value="governance" className="flex min-h-0 flex-1 flex-col">
					<ModelLimitsView />
				</TabsContent>
				<TabsContent value="subscriptions" className="overflow-auto">
					<SubscriptionLimits />
				</TabsContent>
			</Tabs>
		</div>
	);
}