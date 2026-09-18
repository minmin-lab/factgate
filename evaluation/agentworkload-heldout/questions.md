# Held-out questions (one SQL statement each; file hNN.sql)
h01: Total reimbursed amount per city for receipts dated in the second quarter of 2026.
h02: For the Engineering department, the number of receipts and the total amount per expense type in 2026.
h03: The five employees (employee_no, employee_name) with the largest total amount in the Finance department.
h04: The minimum and maximum receipt amount per department.
h05: Expense types whose total amount across all departments exceeds 50000.
h06: Monthly receipt count and total amount for the Sales department in 2026, ordered by month.
h07: Receipts with status 'approved' in the city 'Beijing' with amount above 3000: receipt_no, employee_name, amount, expense_date.
h08: For each department, the number of distinct cities its receipts were filed from.
h09: The percentage of the total 2026 amount contributed by each department.
h10: Receipts whose purpose contains the word 'training' and whose amount is at least 800.
h11: Number of distinct employees per city.
h12: Employees whose count of receipts in 2026 is at least 15, with their receipt counts, highest first.
h13: The earliest expense_date, the latest expense_date, and the total amount for each expense type.
h14: Total amount per week of 2026 (use the expense date).
h15: For each department and city, the total amount, only where the receipt count exceeds 10.
h16: From the monthly summary: total_amount and request_count per department in '2026-03'.
h17: From the monthly summary: the expense type with the highest total_amount in '2026-05'.
h18: From the monthly summary: for each month, the sum of total_amount across all departments and expense types.
h19: From the monthly summary: rows where total_amount exceeds 40 times request_count.
h20: From the monthly summary: for each department, the total_amount of '2026-06' minus the total_amount of '2026-05'.
h21: Number of orders per partition_key.
h22: Total extended price of line items for orders with status 2.
h23: For each partition_key of the orders, the total extended price over its line items (join orders and line items).
h24: The number of line items per order for orders with orderkey at most 50.
h25: Orders with at least ten line items, with their line-item counts.
h26: Total extended price per order status, for orders whose orderkey is at most 5000.
h27: The three orders with the largest total extended price.
h28: Count of line items whose extended price is at most 1000, per partition_key of the line items.
h29: Rows of the heavy result relation with category 'B' and event_date on or after 2026-02-01: row_id, amount, region, revision.
h30: The 50 rows with the largest amount where approved is true.
h31: Rows in region 'AMER' with revision 1 or revision 3: row_id, category, quantity.
h32: For rows with row_id at most 2000: row_id, amount, tax_amount, and the amount including tax (amount plus tax_amount).
h33: Rows whose processed_at is before their event_timestamp: row_id, event_timestamp, processed_at.
h34: Row ids and descriptions of rows whose description ends with 'final'.
h35: All columns of the rows with row_id between 100 and 110.
h36: Total amount per city per expense type in 2026, ordered by city and total descending.
h37: Receipts filed by employees of the Engineering department in the city 'Shenzhen' with status 'pending'.
h38: The number of receipts per expense type and their average amount, ordered by expense type.
h39: Receipts whose amount is greater than twice the average receipt amount of their own expense type.
h40: For each quarter of 2026 (from expense_date), the count of receipts and the sum of amounts for the Finance and Sales departments combined.
