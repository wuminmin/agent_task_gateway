BEGIN;

-- Immutable ordinal companions for the two frozen demo publications. These
-- rows are generated from config/snapshots/*.json by cmd/snapshot-index; the
-- HOT dictionary independently validates every handle/entity-key pairing
-- before an observation can settle.
CREATE SCHEMA taskgate_ordinal;

CREATE TABLE taskgate_ordinal.publications (
    publication_name text PRIMARY KEY,
    manifest_digest text NOT NULL UNIQUE CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    sidecar_digest text NOT NULL UNIQUE CHECK (sidecar_digest ~ '^[0-9a-f]{64}$'),
    row_count bigint NOT NULL CHECK (row_count > 0)
);

INSERT INTO taskgate_ordinal.publications
    (publication_name, manifest_digest, sidecar_digest, row_count)
VALUES
    ('expense-detail-v1',
     '9bc393632756c13ed355181658a6358aa656e1a828f69da36fc951c5061d390c',
     'bbf33dd42179b80cd8cca631f579c4c6e49ef51eb1d3a4ce157090a797c5acf7', 10),
    ('expense-summary-v1',
     '8d675bbf97adfbc03234a468055c223a9ea3d9ffc6ab2d5f6b8da38abeb70432',
     '2cd4e5c835252ec3bcd04aee11c28c70c6329b35bf6770f06a570cd918c9ceff', 10);

CREATE TABLE taskgate_ordinal.expense_detail_v1 (
    row_handle bigint PRIMARY KEY CHECK (row_handle BETWEEN 1 AND 10),
    entity_key char(64) NOT NULL UNIQUE CHECK (entity_key ~ '^[0-9a-f]{64}$'),
    receipt_no text NOT NULL UNIQUE
);

INSERT INTO taskgate_ordinal.expense_detail_v1
    (row_handle, entity_key, receipt_no)
VALUES
    (1,  '27dcfb9ab77118f3586426c9f23b93304fabd3e433042301332221b8bd4e95ec', 'TR-2026-0002'),
    (2,  '3b99408d3227b79a8093d048a8b3789ff30d200b3ec27c512f38a4e37922cc5e', 'TR-2026-0005'),
    (3,  '49a435621ed58eb1abb4577e6afc99392b01ecb172a245a6f24b2234ae6b098b', 'TR-2026-0006'),
    (4,  '79a16f4d633a0d53af563c909ccbfd4f207d4a43a5288f7fa8c7f6cc27987fe1', 'TR-2026-0008'),
    (5,  '80cc832d62a90188bca1402ab90db0268d1216916326f733b149597001c500cb', 'TR-2026-0007'),
    (6,  '912fb58d93acc81afc068ec898d125f5442c8964b4005ffaee8adf06142a879c', 'TR-2026-0010'),
    (7,  '9613abcab6a647eadeac280c04b4fe39d731bc595dee09a894e007e3875ce656', 'TR-2026-0003'),
    (8,  'a58ac6b566bb53ab5be1fcc95515541a09bfb05fb40fba778f0608e1f1c37973', 'TR-2026-0004'),
    (9,  'a9ca0e3c59b36f06555c27f2a7b3a2b2b74a263129a0b3794a65fca9eb32054a', 'TR-2026-0009'),
    (10, 'b6b64311cd71a91c47df7aa13a9a8cae8a09c67b8ca0612d11b835e90a68c244', 'TR-2026-0001');

CREATE TABLE taskgate_ordinal.expense_summary_v1 (
    row_handle bigint PRIMARY KEY CHECK (row_handle BETWEEN 1 AND 10),
    entity_key char(64) NOT NULL UNIQUE CHECK (entity_key ~ '^[0-9a-f]{64}$'),
    month text NOT NULL,
    department text NOT NULL,
    expense_type text NOT NULL,
    UNIQUE (month, department, expense_type)
);

INSERT INTO taskgate_ordinal.expense_summary_v1
    (row_handle, entity_key, month, department, expense_type)
VALUES
    (1,  '195610dcab688e298866363dbdcdccf42f6cab4618a246e3aa18a13525614ba0', '2026-03', '研发部', '酒店'),
    (2,  '296f4dc77ad4a4379186ea908f329a1941930a31a5afaa31342fe77e57367798', '2026-01', '销售部', '机票'),
    (3,  '375a018f289b049715747a6e9d0a870fae88842e3ba9f77459827933824d4570', '2026-02', '销售部', '餐饮'),
    (4,  '537f0ced343bbd26bea57d6d323a11a6a30fae5c38230f5b8e14f34583870520', '2026-01', '销售部', '酒店'),
    (5,  '53cf494a3f049a63a83256b08c0101f69751dc5cf36160a237e445a6b3edd668', '2026-02', '销售部', '高铁'),
    (6,  '65787dccb39cd0c8ac3eab9fce3275859f69beb3eeb2797a46df834ff5e45126', '2026-01', '研发部', '机票'),
    (7,  'bd3ecb387156d85665e139835bf685ab2c0ba53657151324a695e64b206c124f', '2026-03', '销售部', '酒店'),
    (8,  'd258539bdff963b4fc3a6c69078168be037ccd0ea3473b2af000ec3c6f2ca9b6', '2026-04', '研发部', '餐饮'),
    (9,  'de5610c17274e69169d9dcb71b1530903eb70c3dc613b9e3e49e40ff41c75db7', '2026-03', '销售部', '机票'),
    (10, 'f4e1efe6f3be1596b4a85ac5ddb4e2186a81b31596168e8a13294272453cf111', '2026-03', '财务部', '高铁');

REVOKE ALL ON SCHEMA taskgate_ordinal FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA taskgate_ordinal FROM PUBLIC;

COMMIT;
