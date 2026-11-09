import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { expect, setUp, test } from './fixtures';

/** Two tickets, in the body that `whatnow jira push` sends. */
const feed = JSON.stringify({
	tickets: [
		{ key: 'ACME-7', title: 'Fix the login', notes: 'Seen on staging', url: 'https://tracker.example.com/ACME-7' },
		{ key: 'ACME-8', title: 'Tidy the docs', url: 'https://tracker.example.com/ACME-8' }
	]
});

test("A ticket feed's tickets are picked in the morning, and neither a pick nor a skip comes back", async ({ page, request }) => {
	const auth: (string | undefined)[] = [];
	const server = createServer((req, res) => {
		auth.push(req.headers.authorization);
		res.setHeader('Content-Type', 'application/json');
		res.end(feed);
	});
	await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
	const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
	try {
		await setUp(request);

		await page.goto('/settings/integrations');
		const card = page.locator('.card', { hasText: 'Ticket feed' });
		await card.getByRole('button', { name: /Ticket feed/ }).click();
		const address = card.getByLabel('Address');
		const token = card.getByLabel('Token');
		await address.fill(url);
		await token.fill('e2e-token');
		await card.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByText('Saved Ticket feed — switch it on to pull')).toBeVisible();
		await card.getByRole('switch').check();
		// The address is a secret as well as the token, since a feed can carry its token in the path.
		for (const field of [address, token]) {
			await expect(field).toHaveValue('');
			await expect(field).toHaveAttribute('placeholder', 'Leave blank to keep');
		}
		await expect(card.getByRole('button', { name: 'Pull', exact: true })).toBeEnabled();

		await page.goto('/morning');
		const chip = page.getByRole('button', { name: '↻ Ticket feed' });
		await chip.click();
		await expect(page.getByText('Ticket feed · 2')).toBeVisible();
		await page.getByRole('button', { name: 'Skip ACME-8' }).click();
		await page.getByRole('checkbox', { name: 'Pick ACME-7' }).check();
		await page.getByRole('button', { name: 'Add 1' }).click();
		await expect(page.getByText('Added 1.')).toBeVisible();

		await chip.click();
		await expect(page.getByText('Nothing new in Ticket feed.')).toBeVisible();
		expect(auth).toEqual(['Bearer e2e-token', 'Bearer e2e-token']);
	} finally {
		server.close();
	}
});
