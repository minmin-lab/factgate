
SELECT expense_type, COUNT(receipt_no) AS receipt_count, SUM(amount) AS total_amount
FROM expense_detail
WHERE department = 'Engineering'
  AND expense_date >= DATE '2026-01-01'
  AND expense_date <= DATE '2026-12-31'
GROUP BY expense_type
ORDER BY expense_type

